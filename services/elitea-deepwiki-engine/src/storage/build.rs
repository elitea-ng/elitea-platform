//! The build space: stage, publish, reconcile (ADR-0026 decision 5).
//!
//! A generation opens a [`Build`] for one project's wiki (a [`WikiKey`]:
//! `(project_id, wiki_id)`, migration 0005), stages the code graph into the
//! `deepwiki_build` tables with `COPY`, writes its dense vectors to
//! elitea-vector under the build's own generation (the build id,
//! [`crate::storage::dense`]) batch by batch, keeps them for Phase 2 (in
//! memory up to a limit, then in a file in the job's scratch directory,
//! [`BuildVectors`]), and
//! publishes. [`Build::publish`] is ONE transaction:
//!
//! 1. lock the build row (a build the sweep removed cannot publish; the
//!    sweep skips a locked build);
//! 2. queue, without a timeout: first behind a publish of the same wiki of
//!    the same project (a transaction-scoped advisory lock on the project
//!    and the wiki id; two projects publish the same wiki id at once), then
//!    for one of
//!    [`PublishSettings::slots`] publish slots per database (advisory locks
//!    too), which bounds the `work_mem` the publishes take together;
//! 3. set `statement_timeout`, `lock_timeout` and `work_mem` for the rest
//!    of the transaction ([`PublishSettings`]);
//! 4. upsert the `wikis` row, with `active_generation` = this build's id
//!    when it wrote vectors (NULL when it wrote none) and the model and
//!    dimension of those vectors (migrations 0006, 0008), the replaced
//!    active generation added to `wiki_stale_generations`;
//! 5. refuse an empty build, before anything is deleted;
//! 6. delete the wiki's live rows of THIS project: `wiki_bm25_*`, edges,
//!    nodes (rows of other projects are not touched);
//! 7. `INSERT ... SELECT` the staged nodes and edges;
//! 8. write the `wiki_bm25_*` statistics of the `'fts'` branch;
//! 9. delete the build row, which cascades to every staged row.
//!
//! The vectors were written BEFORE the transaction, under a generation no
//! reader names; the commit flips `active_generation` together with the
//! nodes and edges, so a reader (which reads the generation in its own
//! snapshot) searches the old generation with the old nodes or the new one
//! with the new nodes, never a mix. The generation it replaces is added to
//! `wiki_stale_generations` in the same transaction. After the commit
//! exactly the project's listed generations are deleted, in rounds of at
//! most [`STALE_ROUND`], and removed from the list
//! ([`sweep_stale_generations`]), best effort: a failure is logged and the
//! list keeps them for the next holder of a bearer. Builds of one wiki run
//! concurrently (only their publishes are serialised), so "every generation
//! but the active one" would delete the vectors of a build still in flight;
//! the list never names one. A reader whose generation is swept while it
//! searches it notices and searches again (see [`crate::storage::search`]).
//!
//! After the commit, the live tables are `ANALYZE`d, best effort: one
//! short transaction per table with a short `lock_timeout`, skipped (and
//! logged) when another `ANALYZE` or a vacuum holds the table. Inside the
//! publish transaction an `ANALYZE` would hold its lock on the SHARED live
//! tables until the commit and cancel autovacuum on them.
//!
//! Under PostgreSQL's MVCC a reader sees the old index or the new one,
//! never a part of one: no statement of the publish is visible before the
//! commit. `storage/publish.py` had no such guarantee (it committed every
//! batch of 500 nodes).
//!
//! The BM25 statistics are the `'fts'` branch only: the lexemes of the
//! published `fts` column with their position counts, k1 1.2, b 0.75, read
//! back out of the stored tsvectors as
//! `PostgresBackend._rebuild_fts_statistics` does, so the statistics
//! describe exactly what `plainto_tsquery` matches. A document with no
//! lexeme takes no `doc_idx` (the legacy skip rule), `doc_idx` follows
//! `ORDER BY node_id`, and `avgdl` is `total / doc_count`, or 1.0 for an
//! empty corpus. (The legacy standalone `'bm25'` branch, whitespace tokens
//! tokenised while staging, had no reader outside the parity tool and the
//! tests; it is no longer written, and migration 0007 deletes its rows.)
//!
//! Abandoned builds: [`BuildSpace::reconcile_owner`] at startup deletes the
//! builds of this process's owner that an EARLIER run of it opened (each
//! build records the run's boot id, migration 0004), never this run's and
//! never another replica's; [`BuildSpace::sweep`] deletes builds whose
//! heartbeat is older than a limit. An open [`Build`] beats its heartbeat
//! from a background task every [`heartbeat_interval`], so a long model
//! call between staging and the publish does not get it swept; the task
//! stops when the build is dropped, published or abandoned.
//!
//! The vectors of an abandoned build: [`Build::abandon`] deletes its
//! generation from elitea-vector, and so does the worker parent's
//! clean-up of a killed child ([`delete_build`] returns the wiki it
//! belonged to). The reconciliation and the sweep have no caller bearer, so
//! they cannot; they record the removed builds' generations in
//! `wiki_stale_generations` ([`record_stale_generation`]), as does a
//! failed delete, also for a wiki that was never published (it has no
//! `wikis` row). The next publish, abandon or worker clean-up in the
//! project, which has a bearer, deletes them (see
//! [`crate::storage::dense`]).

use crate::graph::{CodeGraph, edge_row, node_row};
use crate::storage::copy::CopyWriter;
use crate::storage::dense::{self, DenseIndex, Space};
use crate::storage::rows::{IndexEdge, IndexNode, collapse_edges};
use crate::storage::text::{BRANCH_FTS, FTS_B, FTS_K1};
use crate::storage::{ProjectScope, Result, StorageError, WikiKey};
use serde_json::Value;
use sqlx::Connection;
use sqlx::postgres::{PgConnection, PgPool};
use std::collections::HashMap;
use std::sync::OnceLock;
use std::time::Duration;

/// The default age after which the sweep removes a build (2 h, ADR-0026).
pub const DEFAULT_STALE_AFTER: Duration = Duration::from_hours(2);

/// The smallest staleness limit the settings accept (5 min): the heartbeat
/// must be able to miss a few beats (a database failover, a long pause)
/// before a live build is swept.
pub const MIN_STALE_AFTER: Duration = Duration::from_mins(5);

/// The advisory lock class of the publish slots
/// (`hashtext(PUBLISH_SLOT_LOCK)`, with the slot number as the object).
pub const PUBLISH_SLOT_LOCK: &str = "elitea_deepwiki.publish_slot";

/// The advisory lock class that orders publishes of one wiki of one project
/// (`hashtext(PUBLISH_WIKI_LOCK)`, with `hashtext(`[`publish_wiki_lock_object`]`)`
/// as the object). The project is part of the key, so two projects
/// publishing the same wiki id do not wait for each other (a hash collision
/// only orders two publishes; it never mixes their rows).
pub const PUBLISH_WIKI_LOCK: &str = "elitea_deepwiki.publish_wiki";

/// The text whose `hashtext` is the object of a publish's
/// [`PUBLISH_WIKI_LOCK`]: `{project_id}/{wiki_id}`.
#[must_use]
pub fn publish_wiki_lock_object(key: &WikiKey) -> String {
    format!("{}/{}", key.project_id(), key.wiki_id())
}

/// How a publish uses the database. Every field has an
/// `ELITEA_DEEPWIKI_PUBLISH_*` setting (`config.rs`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PublishSettings {
    /// `statement_timeout` of each statement of the publish transaction
    /// (default 30 min). A large repository's postings insert took 137 s.
    pub statement_timeout: Duration,
    /// `lock_timeout` of each statement of the publish transaction, after
    /// the queue (default 30 s).
    pub lock_timeout: Duration,
    /// `work_mem` of the publish transaction, in MiB (default 64).
    pub work_mem_mb: u32,
    /// Publishes that run at once against one database (default 2); the
    /// others queue. Every engine replica must use the same number.
    pub slots: u32,
    /// `lock_timeout` of each best-effort `ANALYZE` (default 5 s).
    pub analyze_lock_timeout: Duration,
    /// How long a wiki deletion waits for a publish of the same wiki to
    /// release the per-wiki advisory lock (default 30 s). Past it the
    /// deletion gives up with [`crate::storage::StorageError::Busy`] and the
    /// caller retries; it never queues without a bound.
    pub delete_lock_wait: Duration,
}

impl Default for PublishSettings {
    fn default() -> Self {
        Self {
            statement_timeout: Duration::from_mins(30),
            lock_timeout: Duration::from_secs(30),
            work_mem_mb: 64,
            slots: 2,
            analyze_lock_timeout: Duration::from_secs(5),
            delete_lock_wait: Duration::from_secs(30),
        }
    }
}

impl PublishSettings {
    /// The `SET LOCAL`s of the publish transaction, as `set_config`
    /// arguments. A duration is whole milliseconds, at least 1 (0 would
    /// disable the timeout).
    #[must_use]
    pub fn session_settings(&self) -> [(&'static str, String); 4] {
        [
            ("statement_timeout", millis(self.statement_timeout)),
            ("lock_timeout", millis(self.lock_timeout)),
            ("work_mem", format!("{}MB", self.work_mem_mb.max(1))),
            // Every statement of the publish moves a whole index. With the
            // live tables' statistics stale by a whole wiki, the planner
            // chose nested loops over sequential scans (measured: 23 s for
            // 35k postings against 5k documents); hash joins are right for
            // every one of them.
            ("enable_nestloop", "off".to_owned()),
        ]
    }
}

/// A PostgreSQL duration setting: whole milliseconds, at least 1.
fn millis(duration: Duration) -> String {
    format!("{}ms", duration.as_millis().clamp(1, i32::MAX as u128))
}

/// How often an open build beats its heartbeat: a tenth of the staleness
/// limit, between 100 ms and 1 min (30 s at the 5 min minimum).
#[must_use]
pub fn heartbeat_interval(stale_after: Duration) -> Duration {
    (stale_after / 10).clamp(Duration::from_millis(100), Duration::from_mins(1))
}

/// This process run's boot id, drawn at first use and the same for every
/// [`BuildSpace`] of the process.
#[must_use]
pub fn process_boot_id() -> &'static str {
    static BOOT_ID: OnceLock<String> = OnceLock::new();
    BOOT_ID.get_or_init(new_boot_id)
}

/// A fresh boot id: the process id, the start time and 128 random bits
/// (the standard library's per-process random hash keys).
#[must_use]
pub fn new_boot_id() -> String {
    use std::hash::{BuildHasher, Hasher};
    let random = || {
        std::collections::hash_map::RandomState::new()
            .build_hasher()
            .finish()
    };
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_nanos());
    format!(
        "{:x}-{nanos:x}-{:016x}{:016x}",
        std::process::id(),
        random(),
        random()
    )
}

/// Nodes staged per round of `COPY` statements. Bounds the BM25 postings
/// held in memory while the node `COPY` of the same round is open.
const NODES_PER_ROUND: usize = 2_000;

/// The builds of the `deepwiki_build` schema, for one owner.
#[derive(Debug, Clone)]
pub struct BuildSpace {
    pool: PgPool,
    owner: String,
    boot_id: String,
    stale_after: Duration,
    publish: PublishSettings,
    /// Where the builds write their vectors.
    dense: Option<DenseIndex>,
    /// Where the builds keep their vectors for Phase 2 past a limit.
    spill: Option<VectorSpill>,
}

/// Row counts a stage call wrote.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct StageCounts {
    pub nodes: u64,
    pub edges: u64,
}

/// Row counts a publish wrote into the live tables.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct PublishCounts {
    pub nodes: u64,
    pub edges: u64,
    /// The vectors the build holds (written to elitea-vector before the
    /// transaction; none is a row of this database).
    pub embeddings: u64,
    pub fts_documents: u64,
    /// Every live table was `ANALYZE`d after the commit. `false` when one
    /// was skipped (another `ANALYZE` or a vacuum held it); autovacuum
    /// catches up.
    pub statistics_refreshed: bool,
}

/// The `wikis` row a publish writes: `registry_from_result`'s fields.
///
/// `None` keeps the stored value (or the column default for a new row), as
/// `_update_registry` writes only the fields the caller supplied. A new row
/// without `repo` / `branch` gets `repo = wiki_id`, `branch = 'main'`
/// (`PostgresBackend._ensure_wiki_row`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct WikiRecord {
    pub repo: Option<String>,
    pub branch: Option<String>,
    pub provider: Option<String>,
    pub host: Option<String>,
    pub display_name: Option<String>,
    pub description: Option<String>,
    pub folder_path: Option<String>,
    pub commit_hash: Option<String>,
    pub canonical_repo_identifier: Option<String>,
    pub analysis_key: Option<String>,
    pub wiki_version_id: Option<String>,
    /// The embedding model the build's vectors were made with, and the
    /// dimension the model client's first response established (migration
    /// 0006). Unlike the fields above, `None` is written as NULL: the
    /// publish replaces the wiki's embeddings, so a stored model would
    /// describe vectors that are gone. A build that holds vectors writes the
    /// model and dimension it staged them with
    /// ([`Build::stage_embeddings`]); a different value here is refused.
    pub embedding_model: Option<String>,
    pub embedding_dim: Option<i32>,
}

impl WikiRecord {
    /// `publishing.registry_from_result` over a generation result.
    ///
    /// A value that is not a string reads as absent (Python's `or None`
    /// keeps a non-string; no producer sends one).
    #[must_use]
    pub fn from_result(result: &Value) -> Self {
        let text = |key: &str| {
            result
                .get(key)
                .and_then(Value::as_str)
                .filter(|value| !value.is_empty())
                .map(str::to_owned)
        };
        let canonical = text("canonical_repo_identifier");
        // `canonical.split(":")[0]`: `owner/repo` out of
        // `owner/repo:branch:commit`.
        let repo = canonical
            .as_deref()
            .and_then(|c| c.split(':').next())
            .filter(|r| !r.is_empty())
            .map(str::to_owned);
        let provider = text("provider_type").unwrap_or_else(|| "github".to_owned());
        let host = if provider == "github" || provider == "gitlab" {
            format!("{provider}.com")
        } else {
            provider.clone()
        };
        Self {
            display_name: text("wiki_title").or_else(|| repo.clone()),
            repo,
            branch: text("branch"),
            provider: Some(provider),
            host: Some(host),
            description: text("wiki_description"),
            commit_hash: text("commit_hash"),
            canonical_repo_identifier: canonical,
            analysis_key: text("analysis_key"),
            wiki_version_id: text("wiki_version_id"),
            folder_path: text("wiki_id").map(|id| format!("{id}/")),
            // Set by the publisher from the model client, not by the result.
            embedding_model: None,
            embedding_dim: None,
        }
    }
}

impl BuildSpace {
    /// The build space of `owner`: the identity of this process across a
    /// restart (the pod name), never shared by two live processes. The
    /// boot id is [`process_boot_id`], the staleness limit
    /// [`DEFAULT_STALE_AFTER`], the publish settings the defaults.
    pub fn new(pool: PgPool, owner: impl Into<String>) -> Self {
        Self {
            pool,
            owner: owner.into(),
            boot_id: process_boot_id().to_owned(),
            stale_after: DEFAULT_STALE_AFTER,
            publish: PublishSettings::default(),
            dense: None,
            spill: None,
        }
    }

    /// Keep the builds' vectors for Phase 2 in a file once they pass
    /// `spill.after_bytes` ([`BuildVectors`]). Without it they stay in
    /// memory (the tests, the parity tool).
    #[must_use]
    pub fn with_vector_spill(mut self, spill: Option<VectorSpill>) -> Self {
        self.spill = spill;
        self
    }

    /// The dense store the builds this space opens write their vectors to.
    /// Without one a build keeps its vectors in memory only (Phase 2 reads
    /// them) and publishes no active generation.
    #[must_use]
    pub fn with_dense(mut self, dense: Option<DenseIndex>) -> Self {
        self.dense = dense;
        self
    }

    /// Another boot id (a test stands for a restarted process with it).
    #[must_use]
    pub fn with_boot_id(mut self, boot_id: impl Into<String>) -> Self {
        self.boot_id = boot_id.into();
        self
    }

    /// The staleness limit the sweep uses; a build's heartbeat interval
    /// follows from it ([`heartbeat_interval`]).
    #[must_use]
    pub fn with_stale_after(mut self, stale_after: Duration) -> Self {
        self.stale_after = stale_after;
        self
    }

    /// The publish settings of the builds this space opens.
    #[must_use]
    pub fn with_publish_settings(mut self, publish: PublishSettings) -> Self {
        self.publish = publish;
        self
    }

    /// This process's owner identity.
    #[must_use]
    pub fn owner(&self) -> &str {
        &self.owner
    }

    /// This process run's boot id.
    #[must_use]
    pub fn boot_id(&self) -> &str {
        &self.boot_id
    }

    /// Open a build of one project's wiki, recorded under this owner and
    /// boot id, and start its heartbeat task. The build publishes into that
    /// project only: the pair is recorded on the build row and the publish
    /// reads it from there.
    ///
    /// The build space itself is not scoped: the owner reconciliation and
    /// the sweep delete abandoned BUILDS of any project, never a live index.
    ///
    /// # Errors
    ///
    /// [`StorageError::Database`].
    pub async fn begin(&self, key: &WikiKey) -> Result<Build> {
        let build_id: String = sqlx::query_scalar(
            "INSERT INTO deepwiki_build.builds (build_id, project_id, wiki_id, owner, boot_id) \
             VALUES (gen_random_uuid()::text, $1, $2, $3, $4) RETURNING build_id",
        )
        .bind(key.project_id())
        .bind(key.wiki_id())
        .bind(&self.owner)
        .bind(&self.boot_id)
        .fetch_one(&self.pool)
        .await?;
        let beat_every = heartbeat_interval(self.stale_after);
        let beat = HeartbeatTask::spawn(self.pool.clone(), build_id.clone(), beat_every);
        Ok(Build {
            pool: self.pool.clone(),
            id: build_id,
            key: key.clone(),
            publish: self.publish,
            beat_every,
            beat: Some(beat),
            published: false,
            dense: self.dense.clone(),
            vectors: BuildVectors::new(self.spill.clone()),
        })
    }

    /// Startup reconciliation: delete the builds of this owner that an
    /// earlier run opened (a boot id other than this run's, or none: a
    /// build from before migration 0004). Such a build will never finish.
    /// This run's own builds stay, so the reconciliation is safe at any
    /// time, also when a database that was down at start comes up after
    /// this run opened builds. Returns the builds deleted.
    ///
    /// # Errors
    ///
    /// [`StorageError::Database`].
    pub async fn reconcile_owner(&self) -> Result<u64> {
        let removed: Vec<(String, i32, String)> = sqlx::query_as(
            "DELETE FROM deepwiki_build.builds \
             WHERE owner = $1 AND boot_id IS DISTINCT FROM $2 \
             RETURNING build_id, project_id, wiki_id",
        )
        .bind(&self.owner)
        .bind(&self.boot_id)
        .fetch_all(&self.pool)
        .await?;
        record_orphaned_generations(&self.pool, "reconciliation", &removed).await;
        Ok(removed.len() as u64)
    }

    /// The periodic sweep: delete every build, of any owner, whose
    /// heartbeat is older than `stale_after`. A build that is publishing
    /// holds its row locked and is skipped (`SKIP LOCKED`): the sweep
    /// neither waits for a publish nor removes a build a publish failed
    /// on before its heartbeat resumes. Returns the builds deleted.
    ///
    /// # Errors
    ///
    /// [`StorageError::Database`].
    pub async fn sweep(&self, stale_after: Duration) -> Result<u64> {
        let removed: Vec<(String, i32, String)> = sqlx::query_as(
            "DELETE FROM deepwiki_build.builds WHERE build_id IN ( \
                 SELECT build_id FROM deepwiki_build.builds \
                 WHERE heartbeat_at < now() - make_interval(secs => $1) \
                 FOR UPDATE SKIP LOCKED) \
             RETURNING build_id, project_id, wiki_id",
        )
        .bind(stale_after.as_secs_f64())
        .fetch_all(&self.pool)
        .await?;
        record_orphaned_generations(&self.pool, "sweep", &removed).await;
        Ok(removed.len() as u64)
    }
}

/// The reconciliation and the sweep remove builds without a caller bearer,
/// so the vectors a removed build wrote (its generation) stay in
/// elitea-vector. They are never searched (no reader names them); this
/// records them in `wiki_stale_generations`, which the project's next
/// holder of a bearer deletes. Best effort: a failure is logged.
async fn record_orphaned_generations(pool: &PgPool, by: &str, removed: &[(String, i32, String)]) {
    for (build, project, wiki) in removed {
        tracing::info!(
            by,
            build = %build,
            project,
            wiki = %wiki,
            "removed an abandoned build; its vector generation is deleted by the project's next publish"
        );
        let Some(scope) = crate::storage::ProjectScope::new(*project) else {
            continue;
        };
        if let Err(error) =
            record_stale_generation(pool, &WikiKey::new(scope, wiki.clone()), build).await
        {
            tracing::warn!(build = %build, %error, "could not record the abandoned build's vector generation");
        }
    }
}

/// The most generations one sweep deletes per call to elitea-vector.
pub const STALE_ROUND: usize = 256;

/// The most rounds of [`STALE_ROUND`] one [`sweep_stale_generations`]
/// makes: generations recorded while it runs wait for the next one.
pub const STALE_ROUNDS: usize = 64;

/// Record `generation` of the wiki in `wiki_stale_generations`, for the
/// project's next holder of a bearer to delete
/// ([`sweep_stale_generations`]). Also for a wiki that has no `wikis` row
/// (it was never published). Nothing when it is listed already, or when it
/// is the wiki's active one.
///
/// # Errors
///
/// [`StorageError::Database`].
pub async fn record_stale_generation(pool: &PgPool, key: &WikiKey, generation: &str) -> Result<()> {
    sqlx::query(
        "INSERT INTO wiki_stale_generations (project_id, wiki_id, generation) \
         SELECT $1, $2, $3 WHERE NOT EXISTS ( \
             SELECT 1 FROM wikis \
             WHERE project_id = $1 AND wiki_id = $2 AND active_generation = $3) \
         ON CONFLICT DO NOTHING",
    )
    .bind(key.project_id())
    .bind(key.wiki_id())
    .bind(generation)
    .execute(pool)
    .await?;
    Ok(())
}

/// Delete the generations `wiki_stale_generations` lists for `project`, of
/// every wiki of it, from `store` (whose bearer is admitted for the
/// project), and remove each from the list once it is deleted: rounds of
/// at most [`STALE_ROUND`], at most [`STALE_ROUNDS`] of them. A generation a
/// wiki names as active is skipped (it is listed again, and deleted, once a
/// publish replaces it). Returns the generations deleted. A failed round
/// stops the sweep and stays listed for the next one.
///
/// # Errors
///
/// [`StorageError::Database`]; [`StorageError::Vector`] for the store's
/// refusal (the rounds before it stay deleted and unlisted).
pub async fn sweep_stale_generations(
    pool: &PgPool,
    store: &DenseIndex,
    project: ProjectScope,
) -> Result<usize> {
    let mut deleted = 0;
    for _ in 0..STALE_ROUNDS {
        let listed: Vec<(String, String)> = sqlx::query_as(
            "SELECT s.wiki_id, s.generation FROM wiki_stale_generations s \
             WHERE s.project_id = $1 AND NOT EXISTS ( \
                 SELECT 1 FROM wikis w \
                 WHERE w.project_id = s.project_id AND w.wiki_id = s.wiki_id \
                   AND w.active_generation = s.generation) \
             ORDER BY s.recorded_at, s.wiki_id, s.generation LIMIT $2",
        )
        .bind(project.id())
        .bind(i64::try_from(STALE_ROUND).unwrap_or(i64::MAX))
        .fetch_all(pool)
        .await?;
        if listed.is_empty() {
            break;
        }
        let mut by_wiki: HashMap<String, Vec<String>> = HashMap::new();
        for (wiki, generation) in listed {
            by_wiki.entry(wiki).or_default().push(generation);
        }
        for (wiki, generations) in by_wiki {
            let key = WikiKey::new(project, wiki);
            store
                .delete_generations(&key, generations.clone())
                .await
                .map_err(|e| StorageError::Vector(e.to_string()))?;
            sqlx::query(
                "DELETE FROM wiki_stale_generations \
                 WHERE project_id = $1 AND wiki_id = $2 AND generation = ANY($3)",
            )
            .bind(key.project_id())
            .bind(key.wiki_id())
            .bind(&generations)
            .execute(pool)
            .await?;
            deleted += generations.len();
        }
    }
    Ok(deleted)
}

/// [`sweep_stale_generations`], best effort: a failure is logged, and the
/// generations stay listed for the next holder of a bearer.
pub async fn sweep_stale_generations_logged(
    pool: &PgPool,
    store: &DenseIndex,
    project: ProjectScope,
) {
    match sweep_stale_generations(pool, store, project).await {
        Ok(0) => {}
        Ok(deleted) => tracing::info!(
            project = project.id(),
            deleted,
            "deleted the project's stale vector generations"
        ),
        Err(error) => {
            tracing::warn!(project = project.id(), %error, "could not delete the project's stale vector generations; the next publish retries");
        }
    }
}

/// The background heartbeat of one open build. Dropping it stops it.
#[derive(Debug)]
struct HeartbeatTask(tokio::task::JoinHandle<()>);

impl HeartbeatTask {
    fn spawn(pool: PgPool, build: String, every: Duration) -> Self {
        Self(tokio::spawn(async move {
            loop {
                tokio::time::sleep(every).await;
                match heartbeat(&pool, &build).await {
                    Ok(()) => {}
                    Err(StorageError::Publish(_)) => {
                        tracing::warn!(build, "the build no longer exists; its heartbeat stops");
                        return;
                    }
                    Err(error) => {
                        tracing::warn!(%error, build, "build heartbeat failed; retrying");
                    }
                }
            }
        }))
    }
}

impl Drop for HeartbeatTask {
    fn drop(&mut self) {
        self.0.abort();
    }
}

/// How often the sweep runs: a quarter of the staleness limit, at most
/// every 10 minutes and at least every 10 seconds.
#[must_use]
pub fn sweep_interval(stale_after: Duration) -> Duration {
    (stale_after / 4).clamp(Duration::from_secs(10), Duration::from_mins(10))
}

/// The reconciliation loop `serve` runs when a database is configured:
/// the owner reconciliation once (it spares this run's builds, so a late
/// first success is harmless), then the sweep every [`sweep_interval`]. A failure is logged and retried at the next tick;
/// it never stops the sidecar (no runner needs the database yet, and a
/// database that comes up late is reconciled when it does).
pub async fn run_reconciler(space: BuildSpace, stale_after: Duration) {
    let mut owner_done = false;
    let mut ticker = tokio::time::interval(sweep_interval(stale_after));
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        ticker.tick().await;
        if !owner_done {
            match space.reconcile_owner().await {
                Ok(removed) => {
                    owner_done = true;
                    tracing::info!(owner = %space.owner(), removed, "reconciled this owner's abandoned builds");
                }
                Err(error) => {
                    tracing::warn!(%error, "build reconciliation failed; retrying");
                    continue;
                }
            }
        }
        match space.sweep(stale_after).await {
            Ok(0) => {}
            Ok(removed) => tracing::info!(removed, "swept builds with a stale heartbeat"),
            Err(error) => tracing::warn!(%error, "build sweep failed; retrying"),
        }
    }
}

/// One build in progress. Dropping it stops its heartbeat and leaves its
/// rows for the reconciliation; [`Build::abandon`] deletes them at once.
#[derive(Debug)]
pub struct Build {
    pool: PgPool,
    id: String,
    /// The project and wiki the build publishes into.
    key: WikiKey,
    publish: PublishSettings,
    beat_every: Duration,
    /// `None` while a publish runs and after a publish succeeded.
    beat: Option<HeartbeatTask>,
    published: bool,
    /// Where the vectors go; `None` keeps them in memory only.
    dense: Option<DenseIndex>,
    /// The vectors staged so far.
    vectors: BuildVectors,
}

/// The vector bytes a build holds in memory before it spills them (64 MiB:
/// about 6,500 vectors of 2,560 dimensions).
pub const DEFAULT_SPILL_AFTER: usize = 64 << 20;

/// The vector values a [`BuildVectors::scan`] reads per block from a spill
/// file (4 MiB of `f32`), rounded down to whole vectors.
const SCAN_VALUES: usize = 1 << 20;

/// Where a build's vectors go once they pass `after_bytes`: an unlinked
/// file in `directory` (the job's scratch directory), so a large repository
/// does not hold all its vectors in memory for Phase 2.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VectorSpill {
    pub directory: std::path::PathBuf,
    pub after_bytes: usize,
}

impl VectorSpill {
    /// Spill into `directory` past [`DEFAULT_SPILL_AFTER`].
    pub fn new(directory: impl Into<std::path::PathBuf>) -> Self {
        Self {
            directory: directory.into(),
            after_bytes: DEFAULT_SPILL_AFTER,
        }
    }
}

/// Where the vector values are.
#[derive(Debug)]
enum VectorValues {
    Memory(Vec<f32>),
    /// Little-endian `f32`s, `values` of them, in an unlinked file.
    File {
        file: std::fs::File,
        values: usize,
    },
}

/// The vectors a build staged, for Phase 2 (no read-back from the store,
/// whose float16 collections would not give the staged `f32`s back): node
/// ids and one flat `f32` array, `dimension` values per node. In memory up
/// to the build space's [`VectorSpill::after_bytes`], then all of them in a
/// file in the spill directory (memory keeps the ids, never the values), so
/// the values held in memory never pass that limit. The vectors were
/// written to elitea-vector batch by batch already, as they were staged.
#[derive(Debug)]
pub struct BuildVectors {
    model: Option<String>,
    space: Option<Space>,
    ids: Vec<String>,
    positions: HashMap<String, usize>,
    values: VectorValues,
    spill: Option<VectorSpill>,
    /// Points written to the dense store.
    written: u64,
}

impl Default for BuildVectors {
    fn default() -> Self {
        Self::new(None)
    }
}

fn spill_failed(spill: &VectorSpill, error: &std::io::Error) -> StorageError {
    StorageError::Publish(format!(
        "keeping the build's vectors in {} failed: {error}",
        spill.directory.display()
    ))
}

/// An unlinked read-write file in `directory`: removed from the directory at
/// once, so it goes away with the process however it ends.
fn unlinked_file(directory: &std::path::Path) -> std::io::Result<std::fs::File> {
    use std::os::unix::fs::OpenOptionsExt as _;
    let path = directory.join(format!(".build-vectors-{}", new_boot_id()));
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(&path)?;
    if let Err(error) = std::fs::remove_file(&path) {
        tracing::warn!(path = %path.display(), %error, "could not unlink the build's vector file; the job directory's removal takes it");
    }
    Ok(file)
}

fn le_bytes(values: &[f32]) -> Vec<u8> {
    values
        .iter()
        .flat_map(|value| value.to_le_bytes())
        .collect()
}

fn from_le_bytes(bytes: &[u8]) -> impl Iterator<Item = f32> + '_ {
    bytes
        .as_chunks::<4>()
        .0
        .iter()
        .map(|b| f32::from_le_bytes(*b))
}

impl BuildVectors {
    /// No vector yet; past `spill`'s limit the values go to its directory.
    #[must_use]
    pub fn new(spill: Option<VectorSpill>) -> Self {
        Self {
            model: None,
            space: None,
            ids: Vec::new(),
            positions: HashMap::new(),
            values: VectorValues::Memory(Vec::new()),
            spill,
            written: 0,
        }
    }

    /// The vectors held.
    #[must_use]
    pub fn len(&self) -> usize {
        self.ids.len()
    }

    /// Whether no vector is held.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.ids.is_empty()
    }

    /// The dimension, 0 before the first vector.
    #[must_use]
    pub fn dimension(&self) -> usize {
        self.space
            .as_ref()
            .map_or(0, |space| space.dimension as usize)
    }

    /// The model the vectors were made with.
    #[must_use]
    pub fn model(&self) -> Option<&str> {
        self.model.as_deref()
    }

    /// The node ids, in staging order.
    #[must_use]
    pub fn ids(&self) -> &[String] {
        &self.ids
    }

    /// Whether the values are in the spill file.
    #[must_use]
    pub fn spilled(&self) -> bool {
        matches!(self.values, VectorValues::File { .. })
    }

    /// The bytes of vector values held in memory.
    #[must_use]
    pub fn held_in_memory(&self) -> usize {
        match &self.values {
            VectorValues::Memory(values) => values.len() * size_of::<f32>(),
            VectorValues::File { .. } => 0,
        }
    }

    /// Append `values` (whole vectors), spilling when they pass the limit.
    fn append(&mut self, values: &[f32]) -> Result<()> {
        if let VectorValues::Memory(held) = &mut self.values {
            let bytes = (held.len() + values.len()) * size_of::<f32>();
            match &self.spill {
                Some(spill) if bytes > spill.after_bytes => {
                    let file = unlinked_file(&spill.directory)
                        .map_err(|error| spill_failed(spill, &error))?;
                    let held = std::mem::take(held);
                    self.values = VectorValues::File { file, values: 0 };
                    for block in held.chunks(SCAN_VALUES) {
                        self.append(block)?;
                    }
                }
                _ => {
                    held.extend_from_slice(values);
                    return Ok(());
                }
            }
        }
        if let VectorValues::File { file, values: held } = &mut self.values {
            use std::os::unix::fs::FileExt as _;
            let offset = (*held * size_of::<f32>()) as u64;
            if let Err(error) = file.write_all_at(&le_bytes(values), offset) {
                return Err(match &self.spill {
                    Some(spill) => spill_failed(spill, &error),
                    None => StorageError::Publish(format!(
                        "keeping the build's vectors failed: {error}"
                    )),
                });
            }
            *held += values.len();
        }
        Ok(())
    }

    /// The vector of `node_id`.
    ///
    /// # Errors
    ///
    /// The spill file's read failure.
    pub fn get(&self, node_id: &str) -> std::io::Result<Option<Vec<f32>>> {
        let dimension = self.dimension();
        let Some(position) = self.positions.get(node_id) else {
            return Ok(None);
        };
        match &self.values {
            VectorValues::Memory(values) => Ok(Some(
                values[position * dimension..(position + 1) * dimension].to_vec(),
            )),
            VectorValues::File { file, .. } => {
                use std::os::unix::fs::FileExt as _;
                let mut bytes = vec![0_u8; dimension * size_of::<f32>()];
                file.read_exact_at(&mut bytes, (position * dimension * size_of::<f32>()) as u64)?;
                Ok(Some(from_le_bytes(&bytes).collect()))
            }
        }
    }

    /// Every vector, in staging order (the order of [`Self::ids`]), in
    /// blocks of whole vectors: `visit(index of the block's first vector,
    /// its values)`. From the spill file a block holds at most 4 MiB.
    ///
    /// # Errors
    ///
    /// The spill file's read failure.
    pub fn scan(&self, mut visit: impl FnMut(usize, &[f32])) -> std::io::Result<()> {
        let dimension = self.dimension();
        if dimension == 0 {
            return Ok(());
        }
        let per_block = (SCAN_VALUES / dimension).max(1);
        match &self.values {
            VectorValues::Memory(values) => {
                for (block, chunk) in values.chunks(per_block * dimension).enumerate() {
                    visit(block * per_block, chunk);
                }
            }
            VectorValues::File { file, values } => {
                use std::os::unix::fs::FileExt as _;
                let mut bytes = Vec::new();
                let mut decoded = Vec::new();
                let mut first = 0;
                while first * dimension < *values {
                    let count = per_block.min(*values / dimension - first);
                    bytes.resize(count * dimension * size_of::<f32>(), 0);
                    file.read_exact_at(&mut bytes, (first * dimension * size_of::<f32>()) as u64)?;
                    decoded.clear();
                    decoded.extend(from_le_bytes(&bytes));
                    visit(first, &decoded);
                    first += count;
                }
            }
        }
        Ok(())
    }
}

/// Rows per cluster-column `UPDATE` ([`Build::set_clusters`]).
const CLUSTER_ROUND: usize = 10_000;

/// Delete the build `build_id` and, by cascade, everything it staged, with
/// a [`DELETE_LOCK_TIMEOUT`] and a [`DELETE_STATEMENT_TIMEOUT`]. The
/// generation worker's parent calls this after the child ended, so a child
/// that was killed before it could abandon its build leaves no staging
/// rows. Returns the wiki the build belonged to when a build was deleted
/// (`None`: a published or abandoned build is already gone), so the caller
/// can delete the build's vector generation too
/// ([`dense::DenseIndex::delete_generations`]); a published build's
/// generation is live and is never touched.
///
/// # Errors
///
/// [`StorageError::Database`].
pub async fn delete_build(pool: &PgPool, build_id: &str) -> Result<Option<WikiKey>> {
    let mut tx = pool.begin().await?;
    // Bounded: a killed child's backend can still hold the build row (a
    // publish's `FOR UPDATE`) until the server notices the client is gone.
    // On a timeout the build is left for the sweep.
    apply_settings(
        &mut tx,
        &[
            ("lock_timeout", millis(DELETE_LOCK_TIMEOUT)),
            ("statement_timeout", millis(DELETE_STATEMENT_TIMEOUT)),
        ],
    )
    .await?;
    let deleted: Option<(i32, String)> = sqlx::query_as(
        "DELETE FROM deepwiki_build.builds WHERE build_id = $1 RETURNING project_id, wiki_id",
    )
    .bind(build_id)
    .fetch_optional(&mut *tx)
    .await?;
    tx.commit().await?;
    Ok(deleted.and_then(|(project, wiki)| {
        crate::storage::ProjectScope::new(project).map(|scope| WikiKey::new(scope, wiki))
    }))
}

/// `lock_timeout` of [`delete_build`] (15 s): longer than a dead worker's
/// backend takes to notice its client is gone
/// ([`super::CLIENT_CHECK_INTERVAL_MS`]).
pub const DELETE_LOCK_TIMEOUT: Duration = Duration::from_secs(15);

/// `statement_timeout` of [`delete_build`] (2 min): the cascade over a
/// large build's staged rows.
pub const DELETE_STATEMENT_TIMEOUT: Duration = Duration::from_mins(2);

/// The node `COPY`'s column list (no `fts`: it is generated).
const NODE_COPY: &str = "COPY deepwiki_build.wiki_nodes (build_id, node_id, rel_path, file_name, \
     language, start_line, end_line, symbol_name, symbol_type, parent_symbol, source_text, \
     docstring, signature, chunk_type, macro_cluster, micro_cluster, is_architectural, is_doc, \
     is_test) FROM STDIN";
const EDGE_COPY: &str = "COPY deepwiki_build.wiki_edges (build_id, source_id, target_id, \
     rel_type, edge_class, weight) FROM STDIN";

/// The staged tables, `ANALYZE`d before a publish reads them.
const STAGING_TABLES: [&str; 2] = ["deepwiki_build.wiki_nodes", "deepwiki_build.wiki_edges"];

/// The live tables, `ANALYZE`d after a publish commits.
pub const LIVE_TABLES: [&str; 6] = [
    "wiki_nodes",
    "wiki_edges",
    "wiki_bm25_meta",
    "wiki_bm25_docs",
    "wiki_bm25_terms",
    "wiki_bm25_postings",
];

impl Build {
    /// The build's id.
    #[must_use]
    pub fn build_id(&self) -> &str {
        &self.id
    }

    /// The wiki the build publishes.
    #[must_use]
    pub fn wiki_id(&self) -> &str {
        self.key.wiki_id()
    }

    /// The project and wiki the build publishes into.
    #[must_use]
    pub fn key(&self) -> &WikiKey {
        &self.key
    }

    /// Record that the build is alive. The background task does this every
    /// [`heartbeat_interval`]; a call here beats at once. Fails when the
    /// build no longer exists (the sweep removed it, or it was published):
    /// its work cannot be published.
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] when the build is gone;
    /// [`StorageError::Database`].
    pub async fn heartbeat(&self) -> Result<()> {
        heartbeat(&self.pool, &self.id).await
    }

    /// Stage the whole code graph: its nodes (in graph order, one row at a
    /// time, so the rows are never all held) and its edges, collapsed.
    ///
    /// # Errors
    ///
    /// See [`Build::stage_nodes`] and [`Build::stage_edges`].
    pub async fn stage_graph(&mut self, graph: &CodeGraph) -> Result<StageCounts> {
        let mut nodes = Vec::with_capacity(NODES_PER_ROUND);
        let mut counts = StageCounts::default();
        for (id, data) in graph.nodes() {
            nodes.push(IndexNode::from_row(node_row(id, data))?);
            if nodes.len() == NODES_PER_ROUND {
                counts = add(counts, self.stage_nodes(std::mem::take(&mut nodes)).await?);
            }
        }
        if !nodes.is_empty() {
            counts = add(counts, self.stage_nodes(nodes).await?);
        }
        let edges = collapse_edges(
            graph
                .edges()
                .map(|edge| IndexEdge::from_row(edge_row(edge))),
        );
        counts.edges = self.stage_edges(&edges).await?;
        Ok(counts)
    }

    /// Stage nodes. Node ids must be new to this
    /// build (the primary key refuses a repeat).
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] for a build the sweep removed;
    /// [`StorageError::Database`], e.g. for a repeated node id.
    pub async fn stage_nodes(
        &mut self,
        nodes: impl IntoIterator<Item = IndexNode>,
    ) -> Result<StageCounts> {
        self.heartbeat().await?;
        let mut connection = self.pool.acquire().await?;
        let mut counts = StageCounts::default();
        let mut iter = nodes.into_iter().peekable();
        while iter.peek().is_some() {
            let round: Vec<IndexNode> = iter.by_ref().take(NODES_PER_ROUND).collect();
            let mut transaction = connection.begin().await?;
            counts.nodes += copy_nodes(&mut transaction, &self.id, &round).await?;
            transaction.commit().await?;
        }
        Ok(counts)
    }

    /// Stage edges as given (call [`collapse_edges`] first; the primary key
    /// refuses a repeated `(source, target, rel_type)`).
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] / [`StorageError::Database`].
    pub async fn stage_edges(&mut self, edges: &[IndexEdge]) -> Result<u64> {
        self.heartbeat().await?;
        let mut connection = self.pool.acquire().await?;
        let mut writer = CopyWriter::start(&mut connection, EDGE_COPY).await?;
        for edge in edges {
            let encoded = encode_edge(&mut writer, &self.id, edge);
            if let Err(error) = encoded {
                writer.abort("edge encoding failed").await?;
                return Err(error);
            }
            writer.end_row().await?;
        }
        writer.finish().await
    }

    /// Stage dense vectors made with `model`, keyed by node id: keep them
    /// for Phase 2 ([`Build::vectors`], in memory or spilled) and write them to
    /// the dense store under this build's generation, in rounds of at most
    /// 512, BEFORE the publish. Each vector is `f64` as the model client
    /// answers it, stored as `f32` (what pgvector stored too).
    ///
    /// Every vector of a build has the same dimension and model, and names
    /// a node this build staged, once.
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] for an empty or non-finite vector, another
    /// dimension or model than the build's, a node that was not staged or
    /// was given a vector already; [`StorageError::Vector`] when the dense
    /// store refused the write (its message, never a bearer);
    /// [`StorageError::Database`].
    pub async fn stage_embeddings<'a>(
        &mut self,
        model: &str,
        embeddings: impl IntoIterator<Item = (&'a str, &'a [f64])>,
    ) -> Result<u64> {
        self.heartbeat().await?;
        let rows: Vec<(&str, &[f64])> = embeddings.into_iter().collect();
        if rows.is_empty() {
            return Ok(0);
        }
        match self.vectors.model.as_deref() {
            None => self.vectors.model = Some(model.to_owned()),
            Some(staged) if staged == model => {}
            Some(staged) => {
                return Err(StorageError::Publish(format!(
                    "build {} holds vectors of the model '{staged}'; vectors of '{model}' cannot join them",
                    self.id
                )));
            }
        }
        let mut converted: Vec<(&str, Vec<f32>)> = Vec::with_capacity(rows.len());
        for (node_id, vector) in &rows {
            if vector.is_empty() {
                return Err(StorageError::Publish(format!(
                    "embedding {node_id}: an empty embedding cannot be stored"
                )));
            }
            if vector.iter().any(|value| !value.is_finite()) {
                return Err(StorageError::Publish(format!(
                    "embedding {node_id}: the embedding has a non-finite component"
                )));
            }
            #[allow(clippy::cast_possible_truncation)] // f64 -> f32, as pgvector's float4
            converted.push((node_id, vector.iter().map(|v| *v as f32).collect()));
        }
        let dimension = if let Some(space) = &self.vectors.space {
            space.dimension as usize
        } else {
            let space = Space::new(model, converted[0].1.len())
                .map_err(|e| StorageError::Publish(e.to_string()))?;
            let dimension = space.dimension as usize;
            self.vectors.space = Some(space);
            dimension
        };
        let mut seen = std::collections::HashSet::with_capacity(converted.len());
        for (node_id, vector) in &converted {
            if vector.len() != dimension {
                return Err(StorageError::Publish(format!(
                    "embedding {node_id}: {} dimensions, the build's vectors have {dimension}",
                    vector.len()
                )));
            }
            if self.vectors.positions.contains_key(*node_id) || !seen.insert(*node_id) {
                return Err(StorageError::Publish(format!(
                    "embedding {node_id}: the node already has a vector in build {}",
                    self.id
                )));
            }
        }
        let ids: Vec<&str> = converted.iter().map(|(id, _)| *id).collect();
        self.check_staged(&ids).await?;
        if let (Some(store), Some(space)) = (&self.dense, &self.vectors.space) {
            let generation =
                dense::generation_of(&self.id).map_err(|e| StorageError::Vector(e.to_string()))?;
            let points: Vec<(&str, &[f32])> = converted
                .iter()
                .map(|(id, vector)| (*id, vector.as_slice()))
                .collect();
            self.vectors.written += store
                .upsert(&self.key, &generation, space, &points)
                .await
                .map_err(|e| {
                    StorageError::Vector(format!(
                        "writing the vectors to elitea-vector failed: {e}"
                    ))
                })?;
        }
        let flat: Vec<f32> = converted
            .iter()
            .flat_map(|(_, vector)| vector.iter().copied())
            .collect();
        self.vectors.append(&flat)?;
        for (node_id, _) in converted {
            self.vectors
                .positions
                .insert(node_id.to_owned(), self.vectors.ids.len());
            self.vectors.ids.push(node_id.to_owned());
        }
        Ok(rows.len() as u64)
    }

    /// Vectors only for staged nodes, as the staged vector table's foreign
    /// key required (Phase 2 ranks nodes, never bare vectors).
    async fn check_staged(&self, ids: &[&str]) -> Result<()> {
        let staged: Vec<String> = sqlx::query_scalar(
            "SELECT node_id FROM deepwiki_build.wiki_nodes WHERE build_id = $1 AND node_id = ANY($2)",
        )
        .bind(&self.id)
        .bind(ids)
        .fetch_all(&self.pool)
        .await?;
        if staged.len() == ids.len() {
            return Ok(());
        }
        let staged: std::collections::HashSet<&str> = staged.iter().map(String::as_str).collect();
        let missing = ids
            .iter()
            .find(|id| !staged.contains(**id))
            .copied()
            .unwrap_or_default();
        Err(StorageError::Publish(format!(
            "embedding {missing}: build {} staged no such node",
            self.id
        )))
    }

    /// The vectors staged so far.
    #[must_use]
    pub fn vectors(&self) -> &BuildVectors {
        &self.vectors
    }

    /// The generation the build's vectors are written under: the build id.
    #[must_use]
    pub fn generation(&self) -> &str {
        &self.id
    }

    /// Replace the staged edges with `edges` (Phase 2's
    /// `persist_weights_to_db`): delete the build's edges and `COPY` the new
    /// ones, in one transaction, so a failure leaves the old edges. Call
    /// [`collapse_edges`] first (the primary key refuses a repeated
    /// `(source, target, rel_type)`).
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] for a build the sweep removed or an edge
    /// that cannot be encoded; [`StorageError::Database`].
    pub async fn replace_edges(&mut self, edges: &[IndexEdge]) -> Result<u64> {
        self.heartbeat().await?;
        let mut connection = self.pool.acquire().await?;
        let mut transaction = connection.begin().await?;
        sqlx::query("DELETE FROM deepwiki_build.wiki_edges WHERE build_id = $1")
            .bind(&self.id)
            .execute(&mut *transaction)
            .await?;
        let mut writer = CopyWriter::start(&mut transaction, EDGE_COPY).await?;
        for edge in edges {
            let encoded = encode_edge(&mut writer, &self.id, edge);
            if let Err(error) = encoded {
                writer.abort("edge encoding failed").await?;
                return Err(error);
            }
            writer.end_row().await?;
        }
        let written = writer.finish().await?;
        transaction.commit().await?;
        Ok(written)
    }

    /// Set the Phase 3 cluster columns of staged nodes:
    /// `(node_id, macro_cluster, micro_cluster)`, in rounds of
    /// 10,000 rows. A node not listed keeps `NULL`.
    ///
    /// The `UPDATE` rewrites each row, so PostgreSQL recomputes its stored
    /// `fts` column: the price of staging the nodes before Phase 2, whose
    /// lexical search reads them.
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] for a build the sweep removed;
    /// [`StorageError::Database`].
    pub async fn set_clusters(
        &mut self,
        clusters: &[(String, Option<i32>, Option<i32>)],
    ) -> Result<u64> {
        self.heartbeat().await?;
        let mut updated = 0;
        for round in clusters.chunks(CLUSTER_ROUND) {
            let ids: Vec<&str> = round.iter().map(|(id, _, _)| id.as_str()).collect();
            let sections: Vec<Option<i32>> = round.iter().map(|(_, m, _)| *m).collect();
            let pages: Vec<Option<i32>> = round.iter().map(|(_, _, p)| *p).collect();
            updated += sqlx::query(
                "UPDATE deepwiki_build.wiki_nodes AS n \
                 SET macro_cluster = c.macro_cluster, micro_cluster = c.micro_cluster \
                 FROM unnest($2::text[], $3::int4[], $4::int4[]) \
                     AS c(node_id, macro_cluster, micro_cluster) \
                 WHERE n.build_id = $1 AND n.node_id = c.node_id",
            )
            .bind(&self.id)
            .bind(&ids)
            .bind(&sections)
            .bind(&pages)
            .execute(&self.pool)
            .await?
            .rows_affected();
        }
        Ok(updated)
    }

    /// `ANALYZE` the staging tables, best effort (each with the publish's
    /// `analyze_lock_timeout`; a table another `ANALYZE` holds is skipped).
    /// Phase 2 queries the rows just staged, and without statistics that
    /// describe them the planner chooses its plans for empty tables. Returns
    /// whether every table was analyzed.
    pub async fn refresh_statistics(&self) -> bool {
        match self.pool.acquire().await {
            Ok(mut connection) => {
                analyze_best_effort(
                    &mut connection,
                    &STAGING_TABLES,
                    self.publish.analyze_lock_timeout,
                )
                .await
            }
            Err(error) => {
                tracing::warn!(%error, "no connection to refresh the staging statistics");
                false
            }
        }
    }

    /// The pool the build reads and writes through.
    #[must_use]
    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// Delete the build and everything it staged; also after a failed
    /// publish.
    ///
    /// # Errors
    ///
    /// [`StorageError::Database`].
    pub async fn abandon(mut self) -> Result<()> {
        self.beat = None;
        sqlx::query("DELETE FROM deepwiki_build.builds WHERE build_id = $1")
            .bind(&self.id)
            .execute(&self.pool)
            .await?;
        // The generation's points, best effort: a build that wrote none
        // costs one idempotent delete. A failure leaves them to the
        // project's next publish (they are never searched). With the bearer
        // at hand, the project's other stale generations go too.
        if let Some(store) = &self.dense {
            match store
                .delete_generations(&self.key, vec![self.id.clone()])
                .await
            {
                Ok(()) => {
                    sweep_stale_generations_logged(&self.pool, store, self.key.project()).await;
                }
                Err(error) => {
                    tracing::warn!(build = %self.id, %error, "could not delete the abandoned build's vectors; the project's next publish removes them");
                    if let Err(error) =
                        record_stale_generation(&self.pool, &self.key, &self.id).await
                    {
                        tracing::warn!(build = %self.id, %error, "could not record the abandoned build's vector generation");
                    }
                }
            }
        }
        Ok(())
    }

    /// Publish the build as the wiki's live index, in one transaction (see
    /// the module documentation), and remove the build.
    ///
    /// The build stays with the caller. After an error the transaction has
    /// rolled back, the live index is untouched, the build keeps its rows
    /// and its heartbeat runs again: the caller can retry the publish (a
    /// timeout, a lost connection), stage more and retry, or
    /// [`Build::abandon`] it. After a success the build is gone; another
    /// publish, stage or heartbeat is refused.
    ///
    /// # Errors
    ///
    /// [`StorageError::Publish`] when the build is gone or already
    /// published, or staged no node (an empty index is never published
    /// over a possibly good one, the `publish.py` rule);
    /// [`StorageError::Database`], also for a statement or lock timeout.
    pub async fn publish(&mut self, record: &WikiRecord) -> Result<PublishCounts> {
        if self.published {
            return Err(StorageError::Publish(format!(
                "build {} was already published",
                self.id
            )));
        }
        // The publish holds the build row locked: a heartbeat would wait on
        // it, and the sweep skips the build while it is locked.
        self.beat = None;
        let outcome = self.publish_once(record).await;
        match &outcome {
            Ok(_) => self.published = true,
            Err(_) => match heartbeat(&self.pool, &self.id).await {
                Err(StorageError::Publish(_)) => {}
                Ok(()) | Err(_) => {
                    self.beat = Some(HeartbeatTask::spawn(
                        self.pool.clone(),
                        self.id.clone(),
                        self.beat_every,
                    ));
                }
            },
        }
        outcome
    }

    async fn publish_once(&self, record: &WikiRecord) -> Result<PublishCounts> {
        let mut connection = self.pool.acquire().await?;
        // The staging tables were just filled; their statistics describe
        // whatever was there before. Fresh statistics first, outside the
        // transaction and best effort.
        analyze_best_effort(
            &mut connection,
            &STAGING_TABLES,
            self.publish.analyze_lock_timeout,
        )
        .await;
        let build = self.id.as_str();
        let key = &self.key;
        let wiki = key.wiki_id();
        let project = key.project_id();
        let record = self.record_with_vectors(record)?;
        // The generation readers search once this commits: this build's,
        // when it wrote its vectors to the dense store.
        let active_generation = (self.dense.is_some() && !self.vectors.is_empty()).then_some(build);
        let mut tx = connection.begin().await?;
        enter_publish(&mut tx, &self.publish, build, key).await?;

        // The generation this publish replaces is stale from its commit on.
        sqlx::query(
            "INSERT INTO wiki_stale_generations (project_id, wiki_id, generation) \
             SELECT project_id, wiki_id, active_generation FROM wikis \
             WHERE project_id = $1 AND wiki_id = $2 \
               AND active_generation IS NOT NULL \
               AND active_generation IS DISTINCT FROM $3 \
             ON CONFLICT DO NOTHING",
        )
        .bind(project)
        .bind(wiki)
        .bind(active_generation)
        .execute(&mut *tx)
        .await?;
        upsert_wiki(&mut tx, key, &record, active_generation).await?;

        let staged: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM deepwiki_build.wiki_nodes WHERE build_id = $1",
        )
        .bind(build)
        .fetch_one(&mut *tx)
        .await?;
        if staged == 0 {
            return Err(StorageError::Publish(format!(
                "build {build} of {wiki} staged no nodes; refusing to publish an empty index over a possibly-good one"
            )));
        }

        let _replaced = delete_live(&mut tx, key).await?;

        let nodes = sqlx::query(
            "INSERT INTO wiki_nodes (project_id, wiki_id, node_id, rel_path, file_name, language, \
                 start_line, end_line, symbol_name, symbol_type, parent_symbol, source_text, \
                 docstring, signature, chunk_type, macro_cluster, micro_cluster, \
                 is_architectural, is_doc, is_test) \
             SELECT $3, $1, node_id, rel_path, file_name, language, start_line, end_line, \
                 symbol_name, symbol_type, parent_symbol, source_text, docstring, signature, \
                 chunk_type, macro_cluster, micro_cluster, is_architectural, is_doc, is_test \
             FROM deepwiki_build.wiki_nodes WHERE build_id = $2",
        )
        .bind(wiki)
        .bind(build)
        .bind(project)
        .execute(&mut *tx)
        .await?
        .rows_affected();
        let edges = sqlx::query(
            "INSERT INTO wiki_edges (project_id, wiki_id, source_id, target_id, rel_type, \
                 edge_class, weight, metadata) \
             SELECT $3, $1, source_id, target_id, rel_type, edge_class, weight, metadata \
             FROM deepwiki_build.wiki_edges WHERE build_id = $2",
        )
        .bind(wiki)
        .bind(build)
        .bind(project)
        .execute(&mut *tx)
        .await?
        .rows_affected();
        let embeddings = self.vectors.len() as u64;

        // The postings (millions of rows for a large repository) are
        // inserted in key order, so the B-trees fill in order instead of at
        // random (measured on elitea-platform: 5.1M postings, 137 s
        // unsorted); the sorts get `work_mem` for that.
        let fts_documents = write_fts_branch(&mut tx, key).await?;

        sqlx::query("DELETE FROM deepwiki_build.builds WHERE build_id = $1")
            .bind(build)
            .execute(&mut *tx)
            .await?;
        tx.commit().await?;

        // The project's generations listed stale: the previous publish's
        // (added by this transaction) and any recorded before, of any of
        // its wikis. After the commit, so no reader loses its vectors before
        // the flip (one that read the previous snapshot searches again);
        // best effort. Never "all but this one": a concurrent build of the
        // wiki has its own generation in flight.
        if let Some(store) = &self.dense {
            sweep_stale_generations_logged(&self.pool, store, key.project()).await;
        }

        // Statistics that describe the new rows: the first searches after
        // a publish otherwise plan against the old row counts (measured: a
        // BM25 search that had not finished after 10 minutes). After the
        // commit, so the shared tables are not held until it.
        let statistics_refreshed = analyze_best_effort(
            &mut connection,
            &LIVE_TABLES,
            self.publish.analyze_lock_timeout,
        )
        .await;
        Ok(PublishCounts {
            nodes,
            edges,
            embeddings,
            fts_documents,
            statistics_refreshed,
        })
    }

    /// `record` with the model and dimension of the build's vectors. A
    /// build without vectors publishes the record's (normally none).
    fn record_with_vectors(&self, record: &WikiRecord) -> Result<WikiRecord> {
        let mut record = record.clone();
        let (Some(model), dimension) = (self.vectors.model(), self.vectors.dimension()) else {
            return Ok(record);
        };
        let dimension = i32::try_from(dimension).ok();
        let model_differs = record
            .embedding_model
            .as_deref()
            .is_some_and(|m| m != model);
        let dimension_differs = record.embedding_dim.is_some_and(|d| Some(d) != dimension);
        if model_differs || dimension_differs {
            return Err(StorageError::Publish(format!(
                "build {} holds vectors of '{model}' ({} dimensions); the record names another model or dimension",
                self.id,
                self.vectors.dimension()
            )));
        }
        record.embedding_model = Some(model.to_owned());
        record.embedding_dim = dimension;
        Ok(record)
    }
}

/// Delete the live index rows of one project's wiki, and only those:
/// each project keeps its own index of the same wiki id.
///
/// Shared with the wiki deletion ([`crate::storage::delete`]), which runs it
/// under the same [`PUBLISH_WIKI_LOCK`] and returns what it removed.
pub(crate) async fn delete_live(tx: &mut PgConnection, key: &WikiKey) -> Result<LiveRows> {
    let mut removed = LiveRows::default();
    for (table, statement) in [
        (
            "statistics",
            "DELETE FROM wiki_bm25_postings WHERE project_id = $1 AND wiki_id = $2",
        ),
        (
            "statistics",
            "DELETE FROM wiki_bm25_terms WHERE project_id = $1 AND wiki_id = $2",
        ),
        (
            "statistics",
            "DELETE FROM wiki_bm25_docs WHERE project_id = $1 AND wiki_id = $2",
        ),
        (
            "statistics",
            "DELETE FROM wiki_bm25_meta WHERE project_id = $1 AND wiki_id = $2",
        ),
        (
            "edges",
            "DELETE FROM wiki_edges WHERE project_id = $1 AND wiki_id = $2",
        ),
        (
            "nodes",
            "DELETE FROM wiki_nodes WHERE project_id = $1 AND wiki_id = $2",
        ),
    ] {
        let done = sqlx::query(statement)
            .bind(key.project_id())
            .bind(key.wiki_id())
            .execute(&mut *tx)
            .await?
            .rows_affected();
        match table {
            "statistics" => removed.statistics += done,
            "edges" => removed.edges += done,
            _ => removed.nodes += done,
        }
    }
    Ok(removed)
}

/// The live rows `delete_live` removed, by kind.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct LiveRows {
    pub nodes: u64,
    pub edges: u64,
    /// Always 0: the vectors are in elitea-vector, not in this database.
    /// Kept so the deletion results keep their shape.
    pub embeddings: u64,
    /// Rows of the four `wiki_bm25_*` tables together.
    pub statistics: u64,
}

/// The publish transaction's first steps: set the publish settings, lock
/// the build row, queue (without a timeout) behind a publish of the same
/// wiki of the same project and for a publish slot, then set the settings
/// again.
async fn enter_publish(
    tx: &mut PgConnection,
    settings: &PublishSettings,
    build: &str,
    key: &WikiKey,
) -> Result<()> {
    apply_settings(&mut *tx, &settings.session_settings()).await?;

    let locked: Option<(i32, String)> = sqlx::query_as(
        "SELECT project_id, wiki_id FROM deepwiki_build.builds WHERE build_id = $1 FOR UPDATE",
    )
    .bind(build)
    .fetch_optional(&mut *tx)
    .await?;
    let Some((project, wiki)) = locked else {
        return Err(gone(build));
    };
    // The build row records the pair it was opened for; a build is never
    // published into another project's (or another wiki's) index.
    if project != key.project_id() || wiki != key.wiki_id() {
        return Err(StorageError::Publish(format!(
            "build {build} was opened for another project or wiki; refusing to publish it"
        )));
    }

    // The queue: no timeout while waiting for a publish of this wiki,
    // then for a slot. The settings apply again once both are held.
    apply_settings(
        &mut *tx,
        &[
            ("lock_timeout", "0".to_owned()),
            ("statement_timeout", "0".to_owned()),
        ],
    )
    .await?;
    sqlx::query("SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))")
        .bind(PUBLISH_WIKI_LOCK)
        .bind(publish_wiki_lock_object(key))
        .execute(&mut *tx)
        .await?;
    take_publish_slot(&mut *tx, settings.slots, build).await?;
    apply_settings(&mut *tx, &settings.session_settings()).await?;
    Ok(())
}

/// `set_config(name, value, true)` for each pair: `SET LOCAL` with bound
/// values.
pub(crate) async fn apply_settings(
    tx: &mut PgConnection,
    settings: &[(&'static str, String)],
) -> Result<()> {
    for (name, value) in settings {
        sqlx::query("SELECT set_config($1, $2, true)")
            .bind(name)
            .bind(value)
            .execute(&mut *tx)
            .await?;
    }
    Ok(())
}

/// Hold one of `slots` publish slots until the transaction ends. A free
/// slot is taken at once; when every slot is held, the publish waits for
/// the slot its build id hashes to (it queues, it does not fail).
async fn take_publish_slot(tx: &mut PgConnection, slots: u32, build: &str) -> Result<i32> {
    let slots = i32::try_from(slots.max(1)).unwrap_or(i32::MAX);
    for slot in 0..slots {
        let taken: bool = sqlx::query_scalar("SELECT pg_try_advisory_xact_lock(hashtext($1), $2)")
            .bind(PUBLISH_SLOT_LOCK)
            .bind(slot)
            .fetch_one(&mut *tx)
            .await?;
        if taken {
            return Ok(slot);
        }
    }
    let slot = queue_slot(build, slots);
    sqlx::query("SELECT pg_advisory_xact_lock(hashtext($1), $2)")
        .bind(PUBLISH_SLOT_LOCK)
        .bind(slot)
        .execute(&mut *tx)
        .await?;
    Ok(slot)
}

/// The slot a publish waits for when none is free: FNV-1a of the build id,
/// so waiting publishes spread over the slots.
fn queue_slot(build: &str, slots: i32) -> i32 {
    let hash = build.bytes().fold(0xcbf2_9ce4_8422_2325_u64, |hash, byte| {
        (hash ^ u64::from(byte)).wrapping_mul(0x0000_0100_0000_01b3)
    });
    let slots = u64::try_from(slots.max(1)).unwrap_or(1);
    i32::try_from(hash % slots).unwrap_or(0)
}

/// `ANALYZE` each table in its own short transaction with `lock_timeout`.
/// A table another `ANALYZE` or a vacuum holds is skipped and logged.
/// Returns whether every table was analyzed.
async fn analyze_best_effort(
    connection: &mut PgConnection,
    tables: &[&'static str],
    lock_timeout: Duration,
) -> bool {
    let mut all = true;
    for table in tables {
        if let Err(error) = analyze_one(connection, table, lock_timeout).await {
            all = false;
            if is_lock_timeout(&error) {
                tracing::info!(
                    table,
                    "ANALYZE skipped: the table is locked (autovacuum catches up)"
                );
            } else {
                tracing::warn!(table, %error, "ANALYZE failed; autovacuum catches up");
            }
        }
    }
    all
}

async fn analyze_one(
    connection: &mut PgConnection,
    table: &'static str,
    lock_timeout: Duration,
) -> std::result::Result<(), sqlx::Error> {
    let mut tx = connection.begin().await?;
    sqlx::query("SELECT set_config('lock_timeout', $1, true)")
        .bind(millis(lock_timeout))
        .execute(&mut *tx)
        .await?;
    // A table name from the constant lists above, never input.
    // `query`, not `raw_sql`: the `raw_sql` future is not `Send` for every
    // lifetime, which would make the whole publish future non-`Send`.
    let statement = format!("ANALYZE {table}");
    sqlx::query(sqlx::AssertSqlSafe(&*statement))
        .persistent(false)
        .execute(&mut *tx)
        .await?;
    tx.commit().await
}

/// SQLSTATE 55P03 (`lock_not_available`): a `lock_timeout` expired.
#[must_use]
pub fn is_lock_timeout(error: &sqlx::Error) -> bool {
    error
        .as_database_error()
        .and_then(sqlx::error::DatabaseError::code)
        .is_some_and(|code| code == "55P03")
}

fn add(a: StageCounts, b: StageCounts) -> StageCounts {
    StageCounts {
        nodes: a.nodes + b.nodes,
        edges: a.edges + b.edges,
    }
}

fn gone(build: &str) -> StorageError {
    StorageError::Publish(format!(
        "build {build} no longer exists: the reconciliation removed it (its heartbeat was older than the limit, or its owner restarted); its work cannot be published"
    ))
}

async fn heartbeat(pool: &PgPool, build: &str) -> Result<()> {
    let done =
        sqlx::query("UPDATE deepwiki_build.builds SET heartbeat_at = now() WHERE build_id = $1")
            .bind(build)
            .execute(pool)
            .await?;
    if done.rows_affected() == 0 {
        return Err(gone(build));
    }
    Ok(())
}

fn encode_node(writer: &mut CopyWriter<'_>, build: &str, node: &IndexNode) {
    writer.text(build);
    writer.text(&node.node_id);
    writer.text(&node.rel_path);
    writer.text(&node.file_name);
    writer.text(&node.language);
    writer.int(i64::from(node.start_line));
    writer.int(i64::from(node.end_line));
    writer.text(&node.symbol_name);
    writer.text(&node.symbol_type);
    writer.opt_text(node.parent_symbol.as_deref());
    writer.text(&node.source_text);
    writer.text(&node.docstring);
    writer.text(&node.signature);
    writer.opt_text(node.chunk_type.as_deref());
    writer.opt_int(node.macro_cluster.map(i64::from));
    writer.opt_int(node.micro_cluster.map(i64::from));
    writer.boolean(node.is_architectural);
    writer.boolean(node.is_doc);
    writer.boolean(node.is_test);
}

fn encode_edge(writer: &mut CopyWriter<'_>, build: &str, edge: &IndexEdge) -> Result<()> {
    let row = format!("edge {} -> {}", edge.source_id, edge.target_id);
    writer.text(build);
    writer.text(&edge.source_id);
    writer.text(&edge.target_id);
    writer.text(&edge.rel_type);
    writer.opt_text(edge.edge_class.as_deref());
    writer.float(edge.weight, &row)
}

async fn copy_nodes(
    connection: &mut PgConnection,
    build: &str,
    nodes: &[IndexNode],
) -> Result<u64> {
    let mut writer = CopyWriter::start(connection, NODE_COPY).await?;
    for node in nodes {
        encode_node(&mut writer, build, node);
        writer.end_row().await?;
    }
    writer.finish().await
}

/// Insert or update the `wikis` row: `_ensure_wiki_row` then
/// `_update_registry`, in one static statement. A supplied field replaces
/// the stored one; an absent one keeps it (or takes the column default on a
/// new row). `updated_at` is set on every publish.
async fn upsert_wiki(
    tx: &mut PgConnection,
    key: &WikiKey,
    record: &WikiRecord,
    active_generation: Option<&str>,
) -> Result<()> {
    sqlx::query(
        "INSERT INTO wikis (project_id, wiki_id, repo, branch, provider, host, display_name, \
             description, folder_path, commit_hash, canonical_repo_identifier, analysis_key, \
             wiki_version_id, embedding_model, embedding_dim, active_generation, updated_at) \
         VALUES ($13, $1, COALESCE($2, $1), COALESCE($3, 'main'), COALESCE($4, 'github'), \
             COALESCE($5, 'github.com'), COALESCE($6, ''), COALESCE($7, ''), COALESCE($8, ''), \
             $9, $10, $11, $12, $14, $15, $16, now()) \
         ON CONFLICT (project_id, wiki_id) DO UPDATE SET \
             repo = COALESCE($2, wikis.repo), \
             branch = COALESCE($3, wikis.branch), \
             provider = COALESCE($4, wikis.provider), \
             host = COALESCE($5, wikis.host), \
             display_name = COALESCE($6, wikis.display_name), \
             description = COALESCE($7, wikis.description), \
             folder_path = COALESCE($8, wikis.folder_path), \
             commit_hash = COALESCE($9, wikis.commit_hash), \
             canonical_repo_identifier = COALESCE($10, wikis.canonical_repo_identifier), \
             analysis_key = COALESCE($11, wikis.analysis_key), \
             wiki_version_id = COALESCE($12, wikis.wiki_version_id), \
             embedding_model = $14, \
             embedding_dim = $15, \
             active_generation = $16, \
             updated_at = now()",
    )
    .bind(key.wiki_id())
    .bind(record.repo.as_deref())
    .bind(record.branch.as_deref())
    .bind(record.provider.as_deref())
    .bind(record.host.as_deref())
    .bind(record.display_name.as_deref())
    .bind(record.description.as_deref())
    .bind(record.folder_path.as_deref())
    .bind(record.commit_hash.as_deref())
    .bind(record.canonical_repo_identifier.as_deref())
    .bind(record.analysis_key.as_deref())
    .bind(record.wiki_version_id.as_deref())
    .bind(key.project_id())
    .bind(record.embedding_model.as_deref())
    .bind(record.embedding_dim)
    .bind(active_generation)
    .execute(&mut *tx)
    .await?;
    Ok(())
}

/// Write the docs/terms/meta statistics of one branch once its docs and
/// postings are in place (`_store_statistics_from_counters`'s tail).
async fn write_terms_and_meta(
    tx: &mut PgConnection,
    key: &WikiKey,
    branch: &str,
    k1: f64,
    b: f64,
) -> Result<u64> {
    sqlx::query(
        "INSERT INTO wiki_bm25_terms (project_id, wiki_id, branch, term, df) \
         SELECT project_id, wiki_id, branch, term, count(*) FROM wiki_bm25_postings \
         WHERE wiki_id = $1 AND branch = $2 AND project_id = $3 \
         GROUP BY project_id, wiki_id, branch, term",
    )
    .bind(key.wiki_id())
    .bind(branch)
    .bind(key.project_id())
    .execute(&mut *tx)
    .await?;
    // `avgdl = total_length / doc_count if doc_count else 1.0`: one float8
    // division of two exact integers, as Python's.
    let documents: i64 = sqlx::query_scalar(
        "INSERT INTO wiki_bm25_meta (project_id, wiki_id, branch, doc_count, avgdl, k1, b) \
         SELECT $5, $1, $2, count(*)::integer, \
             CASE WHEN count(*) = 0 THEN 1.0::float8 \
                  ELSE sum(length)::float8 / count(*)::float8 END, \
             $3, $4 \
         FROM wiki_bm25_docs WHERE wiki_id = $1 AND branch = $2 AND project_id = $5 \
         RETURNING doc_count::bigint",
    )
    .bind(key.wiki_id())
    .bind(branch)
    .bind(k1)
    .bind(b)
    .bind(key.project_id())
    .fetch_one(&mut *tx)
    .await?;
    Ok(u64::try_from(documents).unwrap_or_default())
}

/// The `'fts'` branch, from the published tsvectors
/// (`_rebuild_fts_statistics`): a document's length is the sum of its
/// lexemes' position counts (1 for a lexeme without positions), its
/// `doc_idx` follows `ORDER BY node_id`.
async fn write_fts_branch(tx: &mut PgConnection, key: &WikiKey) -> Result<u64> {
    let wiki = key.wiki_id();
    sqlx::query(
        "INSERT INTO wiki_bm25_docs (project_id, wiki_id, branch, doc_idx, node_id, length) \
         SELECT $3, $1, $2, (row_number() OVER (ORDER BY node_id) - 1)::integer, node_id, \
             length::integer \
         FROM ( \
             SELECT n.node_id, \
                 coalesce(sum(coalesce(array_length(l.positions, 1), 1)), 0) AS length \
             FROM wiki_nodes n \
             CROSS JOIN LATERAL unnest(n.fts) AS l(lexeme, positions, weights) \
             WHERE n.wiki_id = $1 AND n.project_id = $3 \
             GROUP BY n.node_id \
         ) AS documents \
         WHERE length > 0",
    )
    .bind(wiki)
    .bind(BRANCH_FTS)
    .bind(key.project_id())
    .execute(&mut *tx)
    .await?;
    sqlx::query(
        "INSERT INTO wiki_bm25_postings (project_id, wiki_id, branch, term, doc_idx, tf) \
         SELECT $3, $1, $2, l.lexeme, d.doc_idx, coalesce(array_length(l.positions, 1), 1) \
         FROM wiki_nodes n \
         CROSS JOIN LATERAL unnest(n.fts) AS l(lexeme, positions, weights) \
         JOIN wiki_bm25_docs d \
           ON d.project_id = $3 AND d.wiki_id = $1 AND d.branch = $2 \
          AND d.node_id = n.node_id \
         WHERE n.wiki_id = $1 AND n.project_id = $3 \
         ORDER BY l.lexeme, d.doc_idx",
    )
    .bind(wiki)
    .bind(BRANCH_FTS)
    .bind(key.project_id())
    .execute(&mut *tx)
    .await?;
    write_terms_and_meta(tx, key, BRANCH_FTS, FTS_K1, FTS_B).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    /// The values held in memory never pass the spill limit: past it every
    /// value is in the (unlinked) spill file, and reads give back exactly
    /// what was staged.
    #[test]
    #[allow(clippy::float_cmp)] // exact: the staged f32s come back
    fn vectors_past_the_limit_go_to_the_spill_file() {
        let directory = std::env::temp_dir().join(format!("dw-spill-{}", new_boot_id()));
        std::fs::create_dir_all(&directory).unwrap();
        let dimension = 4;
        let limit = 3 * dimension * size_of::<f32>();
        let mut vectors = BuildVectors::new(Some(VectorSpill {
            directory: directory.clone(),
            after_bytes: limit,
        }));
        vectors.space = Some(Space::new("m", dimension).unwrap());
        let vector = |i: usize| -> Vec<f32> {
            (0..dimension)
                .map(|d| f32::from(u16::try_from(i * 10 + d).unwrap()))
                .collect()
        };
        let mut staged = Vec::new();
        for batch in 0..5 {
            let flat: Vec<f32> = (0..2).flat_map(|i| vector(batch * 2 + i)).collect();
            vectors.append(&flat).unwrap();
            for i in 0..2 {
                let id = format!("n{}", batch * 2 + i);
                vectors.positions.insert(id.clone(), vectors.ids.len());
                vectors.ids.push(id);
            }
            staged.extend(flat);
            assert!(vectors.held_in_memory() <= limit, "batch {batch}");
            assert_eq!(vectors.spilled(), (batch + 1) * 2 > 3, "batch {batch}");
        }
        // Unlinked: nothing is left in the directory.
        assert_eq!(std::fs::read_dir(&directory).unwrap().count(), 0);
        assert_eq!(vectors.get("n7").unwrap(), Some(vector(7)));
        assert_eq!(vectors.get("missing").unwrap(), None);
        let mut scanned = Vec::new();
        vectors
            .scan(|first, block| {
                assert_eq!(first * dimension, scanned.len());
                scanned.extend_from_slice(block);
            })
            .unwrap();
        assert_eq!(scanned, staged);
        std::fs::remove_dir_all(&directory).unwrap();
    }

    /// Without a spill directory the vectors stay in memory.
    #[test]
    fn vectors_without_a_spill_stay_in_memory() {
        let mut vectors = BuildVectors {
            space: Some(Space::new("m", 2).unwrap()),
            ..BuildVectors::default()
        };
        vectors.append(&[1.0; 4096]).unwrap();
        assert!(!vectors.spilled());
        assert_eq!(vectors.held_in_memory(), 4096 * 4);
    }

    #[test]
    fn the_wiki_record_ports_registry_from_result() {
        let record = WikiRecord::from_result(&json!({
            "wiki_id": "acme--notes--main",
            "canonical_repo_identifier": "acme/notes:main:abc1234",
            "branch": "main",
            "provider_type": "gitlab",
            "commit_hash": "abc1234",
            "wiki_title": "",
            "analysis_key": "k",
        }));
        assert_eq!(
            record,
            WikiRecord {
                repo: Some("acme/notes".into()),
                branch: Some("main".into()),
                provider: Some("gitlab".into()),
                host: Some("gitlab.com".into()),
                display_name: Some("acme/notes".into()),
                description: None,
                folder_path: Some("acme--notes--main/".into()),
                commit_hash: Some("abc1234".into()),
                canonical_repo_identifier: Some("acme/notes:main:abc1234".into()),
                analysis_key: Some("k".into()),
                wiki_version_id: None,
                embedding_model: None,
                embedding_dim: None,
            }
        );
        let bare = WikiRecord::from_result(&json!({"provider_type": "bitbucket"}));
        assert_eq!(bare.host.as_deref(), Some("bitbucket"));
        assert_eq!(bare.repo, None);
        assert_eq!(bare.display_name, None);
        assert_eq!(bare.folder_path, None);
    }

    #[test]
    fn the_publish_future_is_send() {
        // A runner spawns the publish; a future that is not `Send` (the
        // `raw_sql` ANALYZE made it so) fails to compile here.
        fn is_send<T: Send>(_: &T) {}
        fn probe(build: &mut Build, record: &WikiRecord) {
            is_send(&build.publish(record));
        }
        let _: fn(&mut Build, &WikiRecord) = probe;
    }

    #[test]
    fn the_publish_session_settings() {
        let settings = PublishSettings::default().session_settings();
        assert_eq!(
            settings,
            [
                ("statement_timeout", "1800000ms".to_owned()),
                ("lock_timeout", "30000ms".to_owned()),
                ("work_mem", "64MB".to_owned()),
                ("enable_nestloop", "off".to_owned()),
            ]
        );
        // A timeout never reaches 0 (which disables it) nor overflows the
        // server's 32-bit milliseconds.
        assert_eq!(millis(Duration::from_micros(10)), "1ms");
        assert_eq!(
            millis(Duration::from_hours(24 * 365)),
            format!("{}ms", i32::MAX)
        );
    }

    #[test]
    fn the_heartbeat_is_well_under_the_staleness_limit() {
        assert_eq!(heartbeat_interval(MIN_STALE_AFTER), Duration::from_secs(30));
        assert_eq!(
            heartbeat_interval(DEFAULT_STALE_AFTER),
            Duration::from_mins(1)
        );
        assert_eq!(
            heartbeat_interval(Duration::from_secs(1)),
            Duration::from_millis(100)
        );
        for stale in [
            MIN_STALE_AFTER,
            DEFAULT_STALE_AFTER,
            Duration::from_hours(24),
        ] {
            assert!(heartbeat_interval(stale) * 5 <= stale, "{stale:?}");
        }
    }

    #[test]
    fn boot_ids_differ_per_run_and_hold_within_one() {
        assert_ne!(new_boot_id(), new_boot_id());
        assert_eq!(process_boot_id(), process_boot_id());
        assert!(process_boot_id().len() >= 32);
    }

    #[test]
    fn a_queued_publish_waits_for_an_existing_slot() {
        for slots in [1, 2, 7] {
            for build in ["a", "b", "0f6e-uuid", ""] {
                assert!((0..slots).contains(&queue_slot(build, slots)));
            }
        }
        assert_eq!(queue_slot("anything", 0), 0);
    }
}
