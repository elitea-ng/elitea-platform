//! The index's [`GraphStore`]: one owner-only SQLite file per workspace
//! (`<app data>/workspaces/<id>/index/index.sqlite`).
//!
//! Schema version 4 (`PRAGMA user_version`; a newer one is refused, the app
//! that wrote it knows it and this one does not; version 1 gains the
//! `embedding_hash` column when it is opened, version 2 the chunk tables,
//! version 3 the chunk vectors):
//!
//! ```sql
//! meta      (key TEXT PRIMARY KEY, value TEXT)            -- schema_version, revision_seq, setting:*,
//!                                                           -- policy (the fingerprint of the last build's policy),
//!                                                           -- drift_files (files changed since the last full build)
//! graphs    (project_id, application_id, revision, attributes, metadata, schema)
//! entities  (project_id, application_id, entity_id, ordinal UNIQUE per graph,
//!            attributes, embedding BLOB NULL, embedding_hash TEXT NULL, attr_hash)
//!                                                          -- embedding_hash: SHA-256 hex of the text the
//!                                                          -- vector was embedded from; NULL = none recorded
//! relations (project_id, application_id, source_id, target_id, ordinal, attributes, attr_hash)
//! documents (project_id, application_id, source_name, key, version, mime, acl, restricted,
//!            size, mtime_ns)                                -- size/mtime: the folder's stat cache
//! sources   (project_id, application_id, toolkit_id, ... the status document's fields)
//! runs      (id, project_id, application_id, toolkit_id, started_at, finished_at, status,
//!            counts, error, tokens)
//! chunks    (id, path, version, line_start, line_end, kind, text, text_hash)
//!                                                          -- the search chunks of each document
//!                                                          -- (`chunks.rs`), at the version the
//!                                                          -- graph was parsed from
//! chunks_fts(text, idents)                                 -- FTS5, contentless, rowid = chunks.id;
//!                                                          -- porter + unicode61; idents: the
//!                                                          -- identifier column
//! vector_sets  (stamp PRIMARY KEY, origin, model, dim, created_at)
//!                                                          -- one per embedder the chunks were
//!                                                          -- embedded with (`local:<model>:<dim>`)
//! chunk_vectors(stamp, text_hash, vector BLOB)             -- int8 `[scale][dim × i8]`, keyed by the
//!                                                          -- chunk text's hash: an unchanged chunk
//!                                                          -- keeps its vector across refreshes
//! ```
//!
//! **Vectors follow the chunks.** They are written by the background
//! embedder after a build, never in a build's transaction; a vector whose
//! text no chunk has any more is pruned by the next pass
//! ([`SqliteGraphStore::prune_vectors`]).
//!
//! **Chunks are committed with the build.** A refresh stages the chunks of
//! every file it read (from exactly the bytes it parsed) as it goes, into a
//! `TEMP` table of the writer connection ([`SqliteGraphStore::stage_chunks`]:
//! a few hundred files at a time, never visible to a reader), and the
//! commit moves them into `chunks` and `chunks_fts` in the same transaction
//! as the graph and the documents, dropping the chunks of documents that
//! are gone: the chunks always describe the committed documents, and no
//! build holds every chunk in memory. `meta.chunks_generation` records the
//! chunker that made them ([`crate::chunks::GENERATION`]).
//!
//! The desktop keeps ONE graph per file ([`GraphKey::LOCAL`]); the key
//! columns are there because the shared conformance suite (and the
//! contract) address graphs by key.
//!
//! **Writes are diffs.** [`GraphStore::complete`] compares each entity's and
//! relation's hash with the stored one and writes only what changed, so a
//! refresh after editing one file writes tens of rows, not the graph.
//! **Ordinals are sparse and only increase**: a kept node keeps its ordinal
//! while it stays in order, and a node the in-memory graph appended (new,
//! or removed and added again) gets the next one. Loading by ordinal gives
//! back exactly the graph's `IndexMap` order, which the in-memory search
//! breaks ties by. Edges are ordered the same way within their source.
//!
//! WAL journal, `synchronous=NORMAL`, one connection behind a mutex (one
//! writer; the desktop's single-instance plugin keeps it one process). The
//! store's [`GraphStore`] futures run their SQLite work on tokio's blocking
//! pool and only await it, so they never hold an async worker (they need a
//! tokio runtime, any flavour). The synchronous methods (`*_now`,
//! [`SqliteGraphStore::complete_with_chunks`], …) block their caller: the
//! index service calls them from its own blocking-pool thread. The trait's
//! writes take a borrowed graph, which the blocking pool cannot borrow:
//! they copy it first, which the service's commits (synchronous) never do.
//!
//! Embeddings are kept beside the attributes, as the PostgreSQL store keeps
//! them, so [`GraphStore::rank`] is a brute-force cosine scan over them.

use crate::chunks::{ChunkKind, ChunkRow};
use crate::fs;
use elitea_content_source::Acl;
use elitea_inventory_core::graph::Graph;
/// The key every call of this store takes (re-exported for its callers).
pub use elitea_inventory_core::store::GraphKey;
use elitea_inventory_core::store::{
    Completion, GraphRead, GraphStore, Imported, Ranking, SourceStatus,
};
use rusqlite::{Connection, OpenFlags, OptionalExtension as _, TransactionBehavior, params};
use serde_json::{Map, Value, json};
use sha2::{Digest as _, Sha256};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

/// `PRAGMA user_version` of the schema this build writes.
pub const SCHEMA_VERSION: i64 = 4;
/// The database's file name in its directory.
pub const FILE_NAME: &str = "index.sqlite";

const SCHEMA: &str = "
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE graphs (
    project_id INTEGER NOT NULL, application_id INTEGER NOT NULL,
    revision INTEGER NOT NULL, attributes TEXT NOT NULL, metadata TEXT NOT NULL, schema TEXT,
    PRIMARY KEY (project_id, application_id));
CREATE TABLE entities (
    project_id INTEGER NOT NULL, application_id INTEGER NOT NULL,
    entity_id TEXT NOT NULL, ordinal INTEGER NOT NULL,
    attributes TEXT NOT NULL, embedding BLOB, embedding_hash TEXT, attr_hash BLOB NOT NULL,
    PRIMARY KEY (project_id, application_id, entity_id),
    UNIQUE (project_id, application_id, ordinal));
CREATE TABLE relations (
    project_id INTEGER NOT NULL, application_id INTEGER NOT NULL,
    source_id TEXT NOT NULL, target_id TEXT NOT NULL, ordinal INTEGER NOT NULL,
    attributes TEXT NOT NULL, attr_hash BLOB NOT NULL,
    PRIMARY KEY (project_id, application_id, source_id, target_id));
CREATE INDEX relations_by_ordinal ON relations (project_id, application_id, ordinal);
CREATE TABLE documents (
    project_id INTEGER NOT NULL, application_id INTEGER NOT NULL,
    source_name TEXT NOT NULL, key TEXT NOT NULL,
    version TEXT NOT NULL, mime TEXT NOT NULL, acl TEXT NOT NULL, restricted INTEGER NOT NULL,
    size INTEGER NOT NULL DEFAULT -1, mtime_ns INTEGER NOT NULL DEFAULT -1,
    PRIMARY KEY (project_id, application_id, source_name, key));
CREATE TABLE sources (
    project_id INTEGER NOT NULL, application_id INTEGER NOT NULL, toolkit_id TEXT NOT NULL,
    toolkit_name TEXT NOT NULL, toolkit_type TEXT NOT NULL, status TEXT NOT NULL,
    started_at TEXT, last_updated TEXT NOT NULL,
    entities_count INTEGER NOT NULL DEFAULT 0, relations_count INTEGER NOT NULL DEFAULT 0,
    documents_processed INTEGER NOT NULL DEFAULT 0,
    error_message TEXT, progress_message TEXT, branch TEXT, commit_sha TEXT,
    PRIMARY KEY (project_id, application_id, toolkit_id));
CREATE TABLE runs (
    id INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL, application_id INTEGER NOT NULL, toolkit_id TEXT NOT NULL,
    started_at TEXT NOT NULL, finished_at TEXT, status TEXT NOT NULL,
    counts TEXT, error TEXT, tokens INTEGER NOT NULL DEFAULT 0);
";

/// The search chunks (schema 3).
const CHUNK_SCHEMA: &str = "
CREATE TABLE chunks (
    id INTEGER PRIMARY KEY, path TEXT NOT NULL, version TEXT NOT NULL,
    line_start INTEGER NOT NULL, line_end INTEGER NOT NULL, kind TEXT NOT NULL,
    text TEXT NOT NULL, text_hash TEXT NOT NULL);
CREATE INDEX chunks_by_path ON chunks (path, line_start);
CREATE VIRTUAL TABLE chunks_fts USING fts5(
    text, idents, content='', contentless_delete=1,
    tokenize='porter unicode61 remove_diacritics 2');
";

/// The chunk vectors (schema 4).
const VECTOR_SCHEMA: &str = "
CREATE TABLE vector_sets (
    stamp TEXT PRIMARY KEY, origin TEXT NOT NULL, model TEXT NOT NULL,
    dim INTEGER NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE chunk_vectors (
    stamp TEXT NOT NULL, text_hash TEXT NOT NULL, vector BLOB NOT NULL,
    PRIMARY KEY (stamp, text_hash)) WITHOUT ROWID;
CREATE INDEX chunks_by_hash ON chunks (text_hash);
";

/// ISO 8601 in UTC with milliseconds, as SQLite writes it.
const NOW: &str = "strftime('%Y-%m-%dT%H:%M:%f', 'now')";

/// A failure of the index store.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    #[error("the index database failed: {0}")]
    Sqlite(#[from] rusqlite::Error),
    /// A graph this store cannot keep (an embedding that is not a list of
    /// numbers). Nothing was written.
    #[error("{0}")]
    Unstorable(String),
    #[error(
        "the index was written by a newer version of the app (schema {0}); update the app or remove the index"
    )]
    NewerSchema(i64),
    /// The directory or a file was refused (a symlink, another user's, inside
    /// the workspace), or could not be created.
    #[error("{0}")]
    Refused(String),
    /// A stored row does not parse.
    #[error("the index is damaged: {0}")]
    Damaged(String),
    #[error("the index is closed")]
    Closed,
}

impl From<std::io::Error> for StoreError {
    fn from(error: std::io::Error) -> Self {
        Self::Refused(error.to_string())
    }
}

type Result<T> = std::result::Result<T, StoreError>;

/// What the store remembers of one document of a source: its version and
/// the `(size, mtime)` it had when that version was computed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocumentStat {
    pub version: String,
    pub size: u64,
    pub mtime_ns: i64,
}

/// The `(size, mtime_ns)` of each document a run hashed or kept, by key.
pub type Stats = HashMap<String, (u64, i64)>;

/// What [`SqliteGraphStore::peek`] reads of an index without loading it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Peek {
    /// The stored settings (`setting:*` in `meta`), by name.
    pub settings: HashMap<String, String>,
    /// Whether a build was ever committed.
    pub built: bool,
    pub entities: u64,
    pub relations: u64,
    pub documents: u64,
    /// When the last completed run finished ([`SqliteGraphStore::last_completed_run`]).
    pub last_run: Option<String>,
    /// The policy fingerprint the last build was committed under.
    pub policy: Option<String>,
    /// Search chunks, and their distinct texts (what embedding them makes
    /// one vector each for).
    pub chunks: u64,
    pub chunk_texts: u64,
    /// Stored chunk vectors by set stamp.
    pub vectors: HashMap<String, u64>,
}

/// One row of the `runs` table: its status and when it finished.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RunRow {
    pub status: String,
    pub finished_at: Option<String>,
    pub error: Option<String>,
}

/// One embedding run, from the `runs` table (`toolkit_id` [`EMBEDDING_RUNS`]).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EmbeddingRun {
    /// `embedding` while it runs, then `embedded`, `capped`,
    /// `budget_exhausted`, `rate_limited`, `error` or `stopped`.
    pub status: String,
    /// Estimated tokens it embedded ([`crate::embed_caps::estimate_tokens`]).
    pub tokens: u64,
    pub error: Option<String>,
}

/// The `runs.toolkit_id` of embedding runs. They share the table with the
/// graph's ingestion runs, which never see them: [`SqliteGraphStore::runs`]
/// leaves them out and none of their statuses is `completed`.
pub const EMBEDDING_RUNS: &str = "embeddings";

/// The chunks a refresh staged for one document: the version they were
/// cut from (the bytes it parsed) and the rows.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Staged {
    pub version: String,
    pub rows: Vec<ChunkRow>,
}

/// The chunks a commit writes: what the refresh staged
/// ([`SqliteGraphStore::stage_chunks`]), and the chunker generation that
/// made them.
#[derive(Debug, Clone, Copy)]
pub struct ChunkUpdate<'a> {
    pub generation: &'a str,
    /// The refresh read every file again (a full rebuild, a new chunker):
    /// every document's chunks are replaced by what was staged, even at an
    /// unchanged version (and dropped when nothing was). Also implied when
    /// the stored chunks are of another generation.
    pub replace: bool,
}

/// The staging table of a build's chunks (the writer connection's own).
const STAGING: &str = "
CREATE TEMP TABLE IF NOT EXISTS chunk_staging (
    path TEXT NOT NULL, version TEXT NOT NULL, line_start INTEGER NOT NULL,
    line_end INTEGER NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL,
    text_hash TEXT NOT NULL, idents TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS temp.chunk_staging_by_path ON chunk_staging (path);
";

/// What a set of chunk vectors was made with: `origin` (`local` or
/// `remote`), the model, and the stored width. Vectors of one set are only
/// ever compared with queries embedded the same way.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VectorSet {
    pub origin: String,
    pub model: String,
    pub dim: usize,
}

impl VectorSet {
    /// `origin:model:dim`.
    #[must_use]
    pub fn stamp(&self) -> String {
        format!("{}:{}:{}", self.origin, self.model, self.dim)
    }
}

/// One stored chunk.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct StoredChunk {
    pub id: i64,
    pub path: String,
    pub line_start: u32,
    pub line_end: u32,
    pub kind: ChunkKind,
    pub text: String,
    pub text_hash: String,
}

/// The right to ingest into one graph, held until dropped.
#[derive(Debug)]
pub struct Lease {
    held: Arc<Mutex<HashSet<GraphKey>>>,
    key: GraphKey,
}

impl Drop for Lease {
    fn drop(&mut self) {
        self.held
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(&self.key);
    }
}

/// The SQLite graph store of one index directory.
///
/// Every field is shared (`Arc`) so that `handle` can hand a copy
/// of the store to the blocking pool: the [`GraphStore`] futures run their
/// SQLite work there, not on the async runtime's threads.
#[derive(Debug)]
pub struct SqliteGraphStore {
    path: PathBuf,
    conn: Arc<Mutex<Option<Connection>>>,
    /// A read-only connection for searches, opened on first use: WAL lets
    /// it read while a refresh writes. Closed with the store.
    reader: Arc<Mutex<Option<Connection>>>,
    closed: Arc<AtomicBool>,
    leases: Arc<Mutex<HashSet<GraphKey>>>,
    last_commit_rows: Arc<AtomicU64>,
    /// How many times a graph was loaded from the database.
    loads: Arc<AtomicU64>,
    /// Bumped by every write that changes which chunks have vectors (a
    /// build, new vectors, a prune): what a cached vector matrix is
    /// checked against.
    vectors_version: Arc<AtomicU64>,
    /// Bumped by every write that changes the chunks or drops vectors (a
    /// build, a prune): a cached matrix of another chunks version holds
    /// chunk ids that may be gone or reused, and is never served.
    chunks_version: Arc<AtomicU64>,
}

fn open_connection(path: &Path) -> Result<Connection> {
    let conn =
        Connection::open_with_flags(path, OpenFlags::default() | OpenFlags::SQLITE_OPEN_NOFOLLOW)?;
    conn.busy_timeout(Duration::from_secs(5))?;
    conn.pragma_update(None, "journal_mode", "WAL")?;
    conn.pragma_update(None, "synchronous", "NORMAL")?;
    // A full build is one large transaction; the WAL it grew is cut back
    // to this size once checkpointed, instead of staying on disk.
    conn.pragma_update(None, "journal_size_limit", 64 * 1024 * 1024)?;
    Ok(conn)
}

fn migrate(conn: &Connection) -> Result<()> {
    let version: i64 = conn.query_row("PRAGMA user_version", [], |row| row.get(0))?;
    match version {
        0 => {
            conn.execute_batch(&format!(
                "BEGIN; {SCHEMA} {CHUNK_SCHEMA} {VECTOR_SCHEMA}
                 INSERT INTO meta (key, value) VALUES ('schema_version', '{SCHEMA_VERSION}');
                 INSERT INTO meta (key, value) VALUES ('revision_seq', '0');
                 PRAGMA user_version = {SCHEMA_VERSION}; COMMIT;"
            ))?;
            Ok(())
        }
        1..=3 => {
            // Version 2 records the hash of each vector's text; version 3
            // adds the search chunks (made by the next refresh); version 4
            // the chunk vectors (made by the background embedder).
            let embedding_hash = if version == 1 {
                "ALTER TABLE entities ADD COLUMN embedding_hash TEXT;"
            } else {
                ""
            };
            let chunks = if version < 3 { CHUNK_SCHEMA } else { "" };
            conn.execute_batch(&format!(
                "BEGIN; {embedding_hash} {chunks} {VECTOR_SCHEMA}
                 UPDATE meta SET value = '{SCHEMA_VERSION}' WHERE key = 'schema_version';
                 PRAGMA user_version = {SCHEMA_VERSION}; COMMIT;"
            ))?;
            Ok(())
        }
        SCHEMA_VERSION => Ok(()),
        newer => Err(StoreError::NewerSchema(newer)),
    }
}

fn sidecars(path: &Path) -> [PathBuf; 2] {
    let name = path
        .file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .unwrap_or_default();
    [
        path.with_file_name(format!("{name}-wal")),
        path.with_file_name(format!("{name}-shm")),
    ]
}

impl SqliteGraphStore {
    /// Open (or create) the index in `dir`: the directory `0700`, the
    /// database created `0600` before SQLite opens it, its `-wal` and `-shm`
    /// checked before and after.
    ///
    /// # Errors
    ///
    /// The directory or a file is refused, the file is not a database, or
    /// it was written by a newer schema.
    pub fn open(dir: &Path) -> Result<Self> {
        fs::create_private_dir(dir)?;
        // Canonical, so the no-follow open refuses a symlinked FILE, not a
        // symlink higher up (macOS's `/var` is one).
        let dir = std::fs::canonicalize(dir)?;
        let path = dir.join(FILE_NAME);
        fs::create_private_file(&path)?;
        for sidecar in sidecars(&path) {
            fs::inspect_private_file(&sidecar)?;
        }
        let conn = open_connection(&path)?;
        migrate(&conn)?;
        for sidecar in sidecars(&path) {
            fs::inspect_private_file(&sidecar)?;
        }
        Ok(Self {
            path,
            conn: Arc::new(Mutex::new(Some(conn))),
            reader: Arc::default(),
            closed: Arc::default(),
            leases: Arc::default(),
            last_commit_rows: Arc::default(),
            loads: Arc::default(),
            vectors_version: Arc::default(),
            chunks_version: Arc::default(),
        })
    }

    /// The same store (its connections, counters and leases), for a
    /// closure the blocking pool runs.
    fn handle(&self) -> Self {
        Self {
            path: self.path.clone(),
            conn: Arc::clone(&self.conn),
            reader: Arc::clone(&self.reader),
            closed: Arc::clone(&self.closed),
            leases: Arc::clone(&self.leases),
            last_commit_rows: Arc::clone(&self.last_commit_rows),
            loads: Arc::clone(&self.loads),
            vectors_version: Arc::clone(&self.vectors_version),
            chunks_version: Arc::clone(&self.chunks_version),
        }
    }

    /// Run `work` on a handle of this store on the blocking pool, and wait
    /// for it without holding an async worker.
    ///
    /// The work is one call of the synchronous store: one lock of the
    /// connection, and each write one transaction. Dropping the returned
    /// future does not stop the work once it has started: it runs to its
    /// end on the blocking pool, so a write is committed whole (or rolled
    /// back on its error) and never left half applied; only its answer is
    /// lost. A panic in `work` resumes in the caller, as it did when the
    /// work ran inline; a runtime shutting down before the work ran
    /// answers [`StoreError::Closed`].
    async fn blocking<T: Send + 'static>(
        &self,
        work: impl FnOnce(&Self) -> Result<T> + Send + 'static,
    ) -> Result<T> {
        let store = self.handle();
        elitea_content_source::blocking_or(move || work(&store), || StoreError::Closed).await
    }

    /// What an index in `dir` holds, read without loading its graph: the
    /// small status read `index_status` answers from before (or without)
    /// opening the index. `None` when there is no index file. The file is
    /// opened with the same checks as [`Self::open`] and closed again.
    ///
    /// # Errors
    ///
    /// As [`Self::open`], or the store failed.
    pub fn peek(dir: &Path) -> Result<Option<Peek>> {
        match std::fs::symlink_metadata(dir.join(FILE_NAME)) {
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => return Err(error.into()),
            Ok(_) => {}
        }
        let store = Self::open(dir)?;
        let peeked = store.with(|conn| {
            let key = GraphKey::LOCAL;
            let mut settings = HashMap::new();
            {
                let mut statement =
                    conn.prepare("SELECT key, value FROM meta WHERE key LIKE 'setting:%'")?;
                let rows = statement.query_map([], |row| {
                    Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
                })?;
                for row in rows {
                    let (name, value) = row?;
                    settings.insert(name.trim_start_matches("setting:").to_owned(), value);
                }
            }
            let built = conn
                .query_row(
                    "SELECT 1 FROM graphs WHERE project_id = ?1 AND application_id = ?2",
                    params![key.project_id, key.application_id],
                    |_| Ok(()),
                )
                .optional()?
                .is_some();
            let count_of = |table: &str| -> Result<u64> {
                let n: i64 = conn.query_row(
                    &format!(
                        "SELECT count(*) FROM {table} WHERE project_id = ?1 AND application_id = ?2"
                    ),
                    params![key.project_id, key.application_id],
                    |row| row.get(0),
                )?;
                Ok(u64::try_from(n).unwrap_or(0))
            };
            Ok(Peek {
                settings,
                built,
                entities: count_of("entities")?,
                relations: count_of("relations")?,
                documents: count_of("documents")?,
                last_run: last_completed_run(conn, key)?,
                policy: policy_of(conn)?,
                chunks: u64::try_from(conn.query_row(
                    "SELECT count(*) FROM chunks",
                    [],
                    |row| row.get::<_, i64>(0),
                )?)
                .unwrap_or(0),
                chunk_texts: u64::try_from(conn.query_row(
                    "SELECT count(DISTINCT text_hash) FROM chunks",
                    [],
                    |row| row.get::<_, i64>(0),
                )?)
                .unwrap_or(0),
                vectors: {
                    let mut statement =
                        conn.prepare("SELECT stamp, count(*) FROM chunk_vectors GROUP BY stamp")?;
                    let rows = statement.query_map([], |row| {
                        Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1)?))
                    })?;
                    let mut vectors = HashMap::new();
                    for row in rows {
                        let (stamp, n) = row?;
                        vectors.insert(stamp, u64::try_from(n).unwrap_or(0));
                    }
                    vectors
                },
            })
        });
        store.close();
        peeked.map(Some)
    }

    /// [`Self::open`], refusing a directory inside `workspace_root`.
    ///
    /// # Errors
    ///
    /// As [`Self::open`], and an index directory inside the workspace.
    pub fn open_for_workspace(dir: &Path, workspace_root: &Path) -> Result<Self> {
        fs::ensure_outside(dir, workspace_root)?;
        Self::open(dir)
    }

    /// The database file.
    #[must_use]
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Fold the WAL into the database and close it; every later call
    /// answers [`StoreError::Closed`]. What a removal does before it
    /// deletes the directory.
    pub fn close(&self) {
        self.closed.store(true, Ordering::SeqCst);
        drop(
            self.reader
                .lock()
                .unwrap_or_else(PoisonError::into_inner)
                .take(),
        );
        let conn = self
            .conn
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .take();
        if let Some(conn) = conn {
            let _ = conn.execute_batch("PRAGMA wal_checkpoint(TRUNCATE);");
            drop(conn);
        }
    }

    /// How many times a graph was loaded from the database (the service
    /// loads only when its in-memory build is not the stored one).
    #[must_use]
    pub fn loads(&self) -> u64 {
        self.loads.load(Ordering::SeqCst)
    }

    /// The fingerprint of the policy the last build was committed under
    /// (`None` before the first build).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn policy(&self) -> Result<Option<String>> {
        self.with(|conn| policy_of(conn))
    }

    /// When the source's last completed run finished, from the runs: a
    /// cancelled or failed run since does not hide it.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn last_completed_run(&self, key: GraphKey) -> Result<Option<String>> {
        self.with(|conn| last_completed_run(conn, key))
    }

    /// Every run of the graph, oldest first.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn runs(&self, key: GraphKey) -> Result<Vec<RunRow>> {
        self.with(|conn| {
            let mut statement = conn.prepare(
                "SELECT status, finished_at, error FROM runs
                  WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id <> ?3
                  ORDER BY id",
            )?;
            let rows = statement.query_map(
                params![key.project_id, key.application_id, EMBEDDING_RUNS],
                |row| {
                    Ok(RunRow {
                        status: row.get(0)?,
                        finished_at: row.get(1)?,
                        error: row.get(2)?,
                    })
                },
            )?;
            Ok(rows.collect::<rusqlite::Result<_>>()?)
        })
    }

    /// Distinct chunk texts with no vector in `stamp` (`None`: every
    /// text, as for an embedder not chosen yet), and their estimated
    /// tokens ([`crate::embed_caps::estimate_tokens`], counted in SQL).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn missing_text_stats(&self, stamp: Option<&str>) -> Result<(u64, u64)> {
        self.read(|conn| {
            let (texts, tokens): (i64, i64) = conn.query_row(
                "SELECT count(*), coalesce(sum((length(t) + 3) / 4), 0) FROM (
                   SELECT c.text AS t FROM chunks c
                    WHERE ?1 IS NULL OR NOT EXISTS (SELECT 1 FROM chunk_vectors v
                                        WHERE v.stamp = ?1 AND v.text_hash = c.text_hash)
                    GROUP BY c.text_hash)",
                [stamp],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )?;
            Ok((
                u64::try_from(texts).unwrap_or(0),
                u64::try_from(tokens).unwrap_or(0),
            ))
        })
    }

    /// The estimated tokens embedded today (UTC), over every run.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn embed_tokens_today(&self) -> Result<u64> {
        self.read(|conn| {
            let today: String =
                conn.query_row("SELECT strftime('%Y-%m-%d', 'now')", [], |row| row.get(0))?;
            let stored: Option<String> = conn
                .query_row(
                    "SELECT value FROM meta WHERE key = 'embed_day_tokens'",
                    [],
                    |row| row.get(0),
                )
                .optional()?;
            Ok(crate::embed_caps::day_total(stored.as_deref(), &today))
        })
    }

    /// Record a batch that embedded `tokens` (estimated): into the run
    /// `run` (a new `embedding` run when `None`) and, when `daily`, today's
    /// counter (what the per-day cap holds; an on-device model is not
    /// counted), in one transaction. Returns the run's id.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn embedding_batch_done(&self, run: Option<i64>, tokens: u64, daily: bool) -> Result<i64> {
        let tokens = i64::try_from(tokens).unwrap_or(i64::MAX);
        self.with(|conn| {
            let tx = conn.transaction()?;
            let run = if let Some(run) = run {
                run
            } else {
                tx.execute(
                    &format!(
                        "INSERT INTO runs (project_id, application_id, toolkit_id,
                                           started_at, status)
                         VALUES (?1, ?2, ?3, {NOW}, 'embedding')"
                    ),
                    params![
                        GraphKey::LOCAL.project_id,
                        GraphKey::LOCAL.application_id,
                        EMBEDDING_RUNS
                    ],
                )?;
                tx.last_insert_rowid()
            };
            tx.execute(
                "UPDATE runs SET tokens = tokens + ?2 WHERE id = ?1",
                params![run, tokens],
            )?;
            if daily {
                let today: String =
                    tx.query_row("SELECT strftime('%Y-%m-%d', 'now')", [], |row| row.get(0))?;
                let stored: Option<String> = tx
                    .query_row(
                        "SELECT value FROM meta WHERE key = 'embed_day_tokens'",
                        [],
                        |row| row.get(0),
                    )
                    .optional()?;
                let total = crate::embed_caps::day_total(stored.as_deref(), &today)
                    .saturating_add(u64::try_from(tokens).unwrap_or(0));
                tx.execute(
                    "INSERT INTO meta (key, value) VALUES ('embed_day_tokens', ?1)
                     ON CONFLICT (key) DO UPDATE SET value = excluded.value",
                    [format!("{today}:{total}")],
                )?;
            }
            tx.commit()?;
            Ok(run)
        })
    }

    /// End embedding run `run` with `status` (and the error that ended it).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn finish_embedding_run(&self, run: i64, status: &str, error: Option<&str>) -> Result<()> {
        self.with(|conn| {
            conn.execute(
                &format!(
                    "UPDATE runs SET status = ?2, error = ?3, finished_at = {NOW} WHERE id = ?1"
                ),
                params![run, status, error],
            )?;
            Ok(())
        })
    }

    /// Every embedding run, oldest first.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn embedding_runs(&self) -> Result<Vec<EmbeddingRun>> {
        self.read(|conn| {
            let mut statement = conn.prepare(
                "SELECT status, tokens, error FROM runs WHERE toolkit_id = ?1 ORDER BY id",
            )?;
            let rows = statement.query_map([EMBEDDING_RUNS], |row| {
                Ok(EmbeddingRun {
                    status: row.get(0)?,
                    tokens: u64::try_from(row.get::<_, i64>(1)?).unwrap_or(0),
                    error: row.get(2)?,
                })
            })?;
            Ok(rows.collect::<rusqlite::Result<_>>()?)
        })
    }

    /// Record that the running refresh was cancelled: its run ends
    /// `cancelled`, and the source's status goes back to what the last
    /// completed run left (`completed`, its time and document count), so
    /// nothing reads the cancellation as a failure or loses the last build's
    /// time. Without a completed run the source is `cancelled`.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn cancel_run(&self, key: GraphKey, toolkit_id: &str) -> Result<()> {
        self.with(|conn| {
            let transaction = conn.transaction()?;
            transaction.execute(
                &format!(
                    "UPDATE runs SET status = 'cancelled', finished_at = {NOW}
                      WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3
                        AND status = 'in_progress'"
                ),
                params![key.project_id, key.application_id, toolkit_id],
            )?;
            let completed: Option<(String, Option<String>)> = transaction
                .query_row(
                    "SELECT finished_at, counts FROM runs
                      WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3
                        AND status = 'completed'
                      ORDER BY id DESC LIMIT 1",
                    params![key.project_id, key.application_id, toolkit_id],
                    |row| Ok((row.get(0)?, row.get(1)?)),
                )
                .optional()?;
            match completed {
                Some((finished_at, counts)) => {
                    let documents = counts
                        .as_deref()
                        .and_then(|counts| serde_json::from_str::<Value>(counts).ok())
                        .and_then(|counts| counts["documents"].as_i64())
                        .unwrap_or(0);
                    transaction.execute(
                        "UPDATE sources SET status = 'completed', last_updated = ?4,
                                documents_processed = ?5, error_message = NULL,
                                progress_message = NULL
                          WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3",
                        params![
                            key.project_id,
                            key.application_id,
                            toolkit_id,
                            finished_at,
                            documents
                        ],
                    )?;
                }
                None => {
                    transaction.execute(
                        &format!(
                            "UPDATE sources SET status = 'cancelled', progress_message = NULL,
                                    last_updated = {NOW}
                              WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3"
                        ),
                        params![key.project_id, key.application_id, toolkit_id],
                    )?;
                }
            }
            transaction.commit()?;
            Ok(())
        })
    }

    /// How many rows the last [`GraphStore::complete`] inserted, updated or
    /// deleted.
    #[must_use]
    pub fn last_commit_rows(&self) -> u64 {
        self.last_commit_rows.load(Ordering::SeqCst)
    }

    /// Forget the chunks staged so far (a build starts; an earlier one may
    /// have failed after staging).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn clear_staged_chunks(&self) -> Result<()> {
        self.with(|conn| {
            conn.execute_batch(STAGING)?;
            conn.execute("DELETE FROM temp.chunk_staging", [])?;
            Ok(())
        })
    }

    /// Stage the chunks of some documents for the next commit with chunks
    /// (one short transaction; a document staged twice keeps the last).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn stage_chunks(&self, staged: &HashMap<String, Staged>) -> Result<()> {
        if staged.is_empty() {
            return Ok(());
        }
        self.with(|conn| {
            conn.execute_batch(STAGING)?;
            let transaction = conn.transaction()?;
            {
                let mut forget =
                    transaction.prepare_cached("DELETE FROM temp.chunk_staging WHERE path = ?1")?;
                let mut insert = transaction.prepare_cached(
                    "INSERT INTO temp.chunk_staging
                         (path, version, line_start, line_end, kind, text, text_hash, idents)
                     VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
                )?;
                for (path, document) in staged {
                    forget.execute([path])?;
                    for row in &document.rows {
                        insert.execute(params![
                            path,
                            document.version,
                            row.line_start,
                            row.line_end,
                            row.kind.as_str(),
                            row.text,
                            row.text_hash,
                            row.idents
                        ])?;
                    }
                }
            }
            transaction.commit()?;
            Ok(())
        })
    }

    fn with<T>(&self, work: impl FnOnce(&mut Connection) -> Result<T>) -> Result<T> {
        let mut guard = self.conn.lock().unwrap_or_else(PoisonError::into_inner);
        let conn = guard.as_mut().ok_or(StoreError::Closed)?;
        work(conn)
    }

    /// Run `work` on the read-only search connection (opened on first use).
    fn read<T>(&self, work: impl FnOnce(&Connection) -> Result<T>) -> Result<T> {
        let mut guard = self.reader.lock().unwrap_or_else(PoisonError::into_inner);
        if self.closed.load(Ordering::SeqCst) {
            return Err(StoreError::Closed);
        }
        if guard.is_none() {
            let conn = Connection::open_with_flags(
                &self.path,
                OpenFlags::SQLITE_OPEN_READ_ONLY
                    | OpenFlags::SQLITE_OPEN_NO_MUTEX
                    | OpenFlags::SQLITE_OPEN_NOFOLLOW,
            )?;
            conn.busy_timeout(Duration::from_secs(5))?;
            *guard = Some(conn);
        }
        let conn = guard.as_ref().ok_or(StoreError::Closed)?;
        work(conn)
    }

    /// The generation of the chunker that made the stored chunks (`None`
    /// before the first build with chunks).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunk_generation(&self) -> Result<Option<String>> {
        self.with(|conn| {
            Ok(conn
                .query_row(
                    "SELECT value FROM meta WHERE key = 'chunks_generation'",
                    [],
                    |row| row.get(0),
                )
                .optional()?)
        })
    }

    /// How many chunks are stored.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunk_count(&self) -> Result<u64> {
        self.read(|conn| {
            let n: i64 = conn.query_row("SELECT count(*) FROM chunks", [], |row| row.get(0))?;
            Ok(u64::try_from(n).unwrap_or(0))
        })
    }

    /// The chunks matching an FTS5 `MATCH` expression, best first (BM25,
    /// the identifier column weighted 0.75 of the text): `(id, score)`,
    /// higher is better. An expression FTS5 refuses (a syntax error) finds
    /// nothing; every other failure (busy, I/O, a damaged index) is an error.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn search_fts(&self, expression: &str, limit: usize) -> Result<Vec<(i64, f64)>> {
        self.read(|conn| {
            let mut statement = conn.prepare_cached(
                "SELECT rowid, bm25(chunks_fts, 1.0, 0.75) FROM chunks_fts
                  WHERE chunks_fts MATCH ?1 ORDER BY bm25(chunks_fts, 1.0, 0.75) LIMIT ?2",
            )?;
            let rows = statement.query_map(
                params![expression, i64::try_from(limit).unwrap_or(i64::MAX)],
                |row| Ok((row.get::<_, i64>(0)?, -row.get::<_, f64>(1)?)),
            );
            match rows.and_then(|rows| rows.collect::<rusqlite::Result<Vec<_>>>()) {
                Ok(found) => Ok(found),
                Err(error) if is_fts_syntax_error(&error) => Ok(Vec::new()),
                Err(error) => Err(error.into()),
            }
        })
    }

    /// The chunks of these ids (any order; missing ones left out).
    ///
    /// # Errors
    ///
    /// The store failed or a row is damaged.
    pub fn chunks_by_id(&self, ids: &[i64]) -> Result<Vec<StoredChunk>> {
        self.read(|conn| {
            let mut statement = conn.prepare_cached(
                "SELECT id, path, line_start, line_end, kind, text, text_hash FROM chunks WHERE id = ?1",
            )?;
            let mut out = Vec::with_capacity(ids.len());
            for id in ids {
                if let Some(chunk) = statement.query_row([id], stored_chunk).optional()? {
                    out.push(chunk?);
                }
            }
            Ok(out)
        })
    }

    /// The chunk of `path` that best covers lines `start..=end` (the most
    /// lines in common, then the tightest); with no lines (`start <= 0`),
    /// the file's first chunk.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunk_covering(&self, path: &str, start: i64, end: i64) -> Result<Option<i64>> {
        self.read(|conn| {
            if start <= 0 {
                let mut statement = conn.prepare_cached(
                    "SELECT id FROM chunks WHERE path = ?1 ORDER BY line_start, id LIMIT 1",
                )?;
                return Ok(statement.query_row([path], |row| row.get(0)).optional()?);
            }
            let end = end.max(start);
            let mut statement = conn.prepare_cached(
                "SELECT id FROM chunks
                  WHERE path = ?1 AND line_start <= ?3 AND line_end >= ?2
                  ORDER BY min(line_end, ?3) - max(line_start, ?2) DESC,
                           line_end - line_start, id
                  LIMIT 1",
            )?;
            Ok(statement
                .query_row(params![path, start, end], |row| row.get(0))
                .optional()?)
        })
    }

    /// Every chunk of one document, in line order.
    ///
    /// # Errors
    ///
    /// The store failed or a row is damaged.
    pub fn chunks_of(&self, path: &str) -> Result<Vec<StoredChunk>> {
        self.read(|conn| {
            let mut statement = conn.prepare_cached(
                "SELECT id, path, line_start, line_end, kind, text, text_hash FROM chunks
                  WHERE path = ?1 ORDER BY line_start, id",
            )?;
            let rows = statement.query_map([path], stored_chunk)?;
            let mut out = Vec::new();
            for row in rows {
                out.push(row??);
            }
            Ok(out)
        })
    }

    /// The version of the vectors (see the field): equal versions, same
    /// vectors.
    #[must_use]
    pub fn vectors_version(&self) -> u64 {
        self.vectors_version.load(Ordering::SeqCst)
    }

    /// The version of the chunks (see the field).
    #[must_use]
    pub fn chunks_version(&self) -> u64 {
        self.chunks_version.load(Ordering::SeqCst)
    }

    /// Record the vector set `set` (idempotent).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn ensure_vector_set(&self, set: &VectorSet) -> Result<()> {
        self.with(|conn| {
            conn.execute(
                &format!(
                    "INSERT INTO vector_sets (stamp, origin, model, dim, created_at)
                     VALUES (?1, ?2, ?3, ?4, {NOW}) ON CONFLICT (stamp) DO NOTHING"
                ),
                params![
                    set.stamp(),
                    set.origin,
                    set.model,
                    i64::try_from(set.dim).unwrap_or(i64::MAX)
                ],
            )?;
            Ok(())
        })
    }

    /// The vector sets stored, with how many vectors each holds.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn vector_sets(&self) -> Result<Vec<(VectorSet, u64)>> {
        self.read(|conn| {
            let mut statement = conn.prepare(
                "SELECT s.origin, s.model, s.dim,
                        (SELECT count(*) FROM chunk_vectors v WHERE v.stamp = s.stamp)
                   FROM vector_sets s ORDER BY s.stamp",
            )?;
            let rows = statement.query_map([], |row| {
                Ok((
                    VectorSet {
                        origin: row.get(0)?,
                        model: row.get(1)?,
                        dim: usize::try_from(row.get::<_, i64>(2)?).unwrap_or(0),
                    },
                    u64::try_from(row.get::<_, i64>(3)?).unwrap_or(0),
                ))
            })?;
            Ok(rows.collect::<rusqlite::Result<_>>()?)
        })
    }

    /// The chunks with no vector in `stamp`: `(id, text_hash)`, one per
    /// distinct text, in id order.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunks_missing_vectors(&self, stamp: &str) -> Result<Vec<(i64, String)>> {
        self.read(|conn| {
            let mut statement = conn.prepare(
                "SELECT min(c.id), c.text_hash FROM chunks c
                  WHERE NOT EXISTS (SELECT 1 FROM chunk_vectors v
                                     WHERE v.stamp = ?1 AND v.text_hash = c.text_hash)
                  GROUP BY c.text_hash ORDER BY min(c.id)",
            )?;
            let rows = statement.query_map([stamp], |row| Ok((row.get(0)?, row.get(1)?)))?;
            Ok(rows.collect::<rusqlite::Result<_>>()?)
        })
    }

    /// The text hashes of the chunks whose estimated tokens
    /// ([`crate::embed_caps::estimate_tokens`], counted in SQL) are more
    /// than `tokens`: texts a token cap that small can never let through.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunk_hashes_over(&self, tokens: u64) -> Result<HashSet<String>> {
        let tokens = i64::try_from(tokens).unwrap_or(i64::MAX);
        self.read(|conn| {
            let mut statement = conn.prepare(
                "SELECT DISTINCT text_hash FROM chunks WHERE (length(text) + 3) / 4 > ?1",
            )?;
            let rows = statement.query_map([tokens], |row| row.get::<_, String>(0))?;
            Ok(rows.collect::<rusqlite::Result<_>>()?)
        })
    }

    /// The text hashes of the chunks of `paths`, each with the rank of its
    /// path in `paths` (the first a hash is found at): what the embedder
    /// takes first.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunk_hashes_of(&self, paths: &[String]) -> Result<HashMap<String, usize>> {
        self.read(|conn| {
            let mut statement =
                conn.prepare_cached("SELECT text_hash FROM chunks WHERE path = ?1")?;
            let mut ranks = HashMap::new();
            for (rank, path) in paths.iter().enumerate() {
                let rows = statement.query_map([path], |row| row.get::<_, String>(0))?;
                for hash in rows {
                    ranks.entry(hash?).or_insert(rank);
                }
            }
            Ok(ranks)
        })
    }

    /// Store vectors of `stamp` by text hash, in one transaction.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn put_vectors(&self, stamp: &str, vectors: &[(String, Vec<u8>)]) -> Result<()> {
        self.with(|conn| {
            let tx = conn.transaction()?;
            {
                let mut insert = tx.prepare_cached(
                    "INSERT INTO chunk_vectors (stamp, text_hash, vector) VALUES (?1, ?2, ?3)
                     ON CONFLICT (stamp, text_hash) DO UPDATE SET vector = excluded.vector",
                )?;
                for (hash, blob) in vectors {
                    insert.execute(params![stamp, hash, blob])?;
                }
            }
            tx.commit()?;
            self.vectors_version.fetch_add(1, Ordering::SeqCst);
            Ok(())
        })
    }

    /// How many chunks have a vector in `stamp`, of how many chunks.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn vector_coverage(&self, stamp: &str) -> Result<(u64, u64)> {
        self.read(|conn| {
            let (with, all): (i64, i64) = conn.query_row(
                "SELECT count(v.text_hash), count(*) FROM chunks c
                   LEFT JOIN chunk_vectors v ON v.stamp = ?1 AND v.text_hash = c.text_hash",
                [stamp],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )?;
            Ok((
                u64::try_from(with).unwrap_or(0),
                u64::try_from(all).unwrap_or(0),
            ))
        })
    }

    /// Every chunk's vector in `stamp`: `(chunk id, blob)`.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn chunk_vectors(&self, stamp: &str) -> Result<Vec<(i64, Vec<u8>)>> {
        self.read(|conn| {
            let mut statement = conn.prepare(
                "SELECT c.id, v.vector FROM chunks c
                   JOIN chunk_vectors v ON v.stamp = ?1 AND v.text_hash = c.text_hash",
            )?;
            let rows = statement.query_map([stamp], |row| Ok((row.get(0)?, row.get(1)?)))?;
            Ok(rows.collect::<rusqlite::Result<_>>()?)
        })
    }

    /// Drop every vector set but `keep` (`None`: all of them), and the
    /// vectors of `keep` whose text no chunk has any more. Returns the
    /// vectors deleted.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn prune_vectors(&self, keep: Option<&str>) -> Result<u64> {
        self.with(|conn| {
            let keep = keep.unwrap_or("");
            let tx = conn.transaction()?;
            let mut deleted = tx.execute("DELETE FROM chunk_vectors WHERE stamp <> ?1", [keep])?;
            tx.execute("DELETE FROM vector_sets WHERE stamp <> ?1", [keep])?;
            deleted += tx.execute(
                "DELETE FROM chunk_vectors WHERE stamp = ?1 AND NOT EXISTS
                   (SELECT 1 FROM chunks c WHERE c.text_hash = chunk_vectors.text_hash)",
                [keep],
            )?;
            tx.commit()?;
            if deleted > 0 {
                self.vectors_version.fetch_add(1, Ordering::SeqCst);
                self.chunks_version.fetch_add(1, Ordering::SeqCst);
            }
            Ok(deleted as u64)
        })
    }

    /// A stored setting.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn setting(&self, name: &str) -> Result<Option<String>> {
        self.read(|conn| {
            Ok(conn
                .query_row(
                    "SELECT value FROM meta WHERE key = ?1",
                    [format!("setting:{name}")],
                    |row| row.get(0),
                )
                .optional()?)
        })
    }

    /// How many files changed since the last full build (`meta.drift_files`;
    /// 0 when never recorded).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn drift_files(&self) -> Result<u64> {
        self.read(|conn| {
            let value: Option<String> = conn
                .query_row(
                    "SELECT value FROM meta WHERE key = 'drift_files'",
                    [],
                    |row| row.get(0),
                )
                .optional()?;
            Ok(value.and_then(|value| value.parse().ok()).unwrap_or(0))
        })
    }

    /// Record [`Self::drift_files`].
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn set_drift_files(&self, files: u64) -> Result<()> {
        self.with(|conn| {
            conn.execute(
                "INSERT INTO meta (key, value) VALUES ('drift_files', ?1)
                 ON CONFLICT (key) DO UPDATE SET value = excluded.value",
                [files.to_string()],
            )?;
            Ok(())
        })
    }

    /// Store a setting.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn set_setting(&self, name: &str, value: &str) -> Result<()> {
        self.with(|conn| {
            conn.execute(
                "INSERT INTO meta (key, value) VALUES (?1, ?2)
                 ON CONFLICT (key) DO UPDATE SET value = excluded.value",
                params![format!("setting:{name}"), value],
            )?;
            Ok(())
        })
    }

    /// [`GraphStore::load`], synchronously (outside an async context).
    ///
    /// # Errors
    ///
    /// The store failed or a row is damaged.
    pub fn load_now(&self, key: GraphKey) -> Result<Option<(Graph, i64)>> {
        self.loads.fetch_add(1, Ordering::SeqCst);
        self.with(|conn| Ok(load_read(conn, key, true)?.map(|read| (read.graph, read.revision))))
    }

    /// [`GraphStore::load_view`], synchronously: the graph without its
    /// vectors (the blobs are never selected), and the embedded ids.
    ///
    /// # Errors
    ///
    /// The store failed or a row is damaged.
    pub fn load_view_now(&self, key: GraphKey) -> Result<Option<GraphRead>> {
        self.loads.fetch_add(1, Ordering::SeqCst);
        self.with(|conn| load_read(conn, key, false))
    }

    /// [`GraphStore::revision`], synchronously.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn revision_now(&self, key: GraphKey) -> Result<Option<i64>> {
        self.with(|conn| {
            Ok(conn
                .query_row(
                    "SELECT revision FROM graphs WHERE project_id = ?1 AND application_id = ?2",
                    params![key.project_id, key.application_id],
                    |row| row.get(0),
                )
                .optional()?)
        })
    }

    /// [`GraphStore::status_document`], synchronously.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn status_document_now(&self, key: GraphKey) -> Result<Value> {
        self.with(|conn| status_document(conn, key))
    }

    /// What the source's last completed run recorded of each document: the
    /// stat cache a folder listing compares against before it re-hashes.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn document_stats(
        &self,
        key: GraphKey,
        source_name: &str,
    ) -> Result<HashMap<String, DocumentStat>> {
        self.with(|conn| {
            let mut statement = conn.prepare(
                "SELECT key, version, size, mtime_ns FROM documents
                  WHERE project_id = ?1 AND application_id = ?2 AND source_name = ?3",
            )?;
            let rows = statement.query_map(
                params![key.project_id, key.application_id, source_name],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, i64>(2)?,
                        row.get::<_, i64>(3)?,
                    ))
                },
            )?;
            let mut stats = HashMap::new();
            for row in rows {
                let (document, version, size, mtime_ns) = row?;
                // -1: recorded without a stat (it never matches one).
                if let Ok(size) = u64::try_from(size) {
                    stats.insert(
                        document,
                        DocumentStat {
                            version,
                            size,
                            mtime_ns,
                        },
                    );
                }
            }
            Ok(stats)
        })
    }

    /// [`GraphStore::complete`] that also records each document's
    /// `(size, mtime)` from `stats`, in the same transaction: the stat cache
    /// is only ever the one the committed versions were computed with. With
    /// `policy`, the fingerprint of the policy the build was made under is
    /// recorded in the same transaction too ([`Self::policy`]).
    ///
    /// # Errors
    ///
    /// As [`GraphStore::complete`].
    pub fn complete_with_stats(
        &self,
        key: GraphKey,
        graph: &Graph,
        completion: &Completion<'_>,
        stats: &Stats,
        policy: Option<&str>,
    ) -> Result<i64> {
        self.complete_with_chunks(key, graph, completion, stats, policy, None)
    }

    /// [`Self::complete_with_stats`] that also writes the staged chunks in
    /// the same transaction (see the module's "Chunks are committed with
    /// the build").
    ///
    /// # Errors
    ///
    /// As [`GraphStore::complete`].
    pub fn complete_with_chunks(
        &self,
        key: GraphKey,
        graph: &Graph,
        completion: &Completion<'_>,
        stats: &Stats,
        policy: Option<&str>,
        chunks: Option<ChunkUpdate<'_>>,
    ) -> Result<i64> {
        check_rows(graph)?;
        let written = Written {
            stats,
            policy,
            chunks,
        };
        let (revision, rows) = self.with(|conn| commit(conn, key, graph, completion, &written))?;
        self.last_commit_rows.store(rows, Ordering::SeqCst);
        self.vectors_version.fetch_add(1, Ordering::SeqCst);
        self.chunks_version.fetch_add(1, Ordering::SeqCst);
        Ok(revision)
    }
}

// ----------------------------------------------------------------- rows

struct EntityRow {
    id: String,
    attributes: String,
    embedding: Option<Vec<u8>>,
    /// The hash of the text the embedding was made from.
    embedding_hash: Option<String>,
    hash: Vec<u8>,
}

struct RelationRow {
    source: String,
    target: String,
    attributes: String,
    hash: Vec<u8>,
}

fn hash(attributes: &str, embedding: Option<&[u8]>, embedding_hash: Option<&str>) -> Vec<u8> {
    let mut digest = Sha256::new();
    digest.update(attributes.as_bytes());
    if let Some(embedding) = embedding {
        digest.update([0xff]);
        digest.update(embedding);
    }
    if let Some(embedding_hash) = embedding_hash {
        digest.update([0xfe]);
        digest.update(embedding_hash.as_bytes());
    }
    digest.finalize().to_vec()
}

fn to_text(value: &Value) -> Result<String> {
    serde_json::to_string(value).map_err(|error| StoreError::Unstorable(error.to_string()))
}

fn encode(vector: &[f64]) -> Vec<u8> {
    vector
        .iter()
        .flat_map(|value| value.to_le_bytes())
        .collect()
}

fn decode(bytes: &[u8]) -> Vec<f64> {
    bytes
        .as_chunks::<8>()
        .0
        .iter()
        .map(|chunk| f64::from_le_bytes(*chunk))
        .collect()
}

/// One node's row, the embedding split off its attributes (made as it is
/// written: a commit never holds every serialised row).
fn entity_row(graph: &Graph, id: &str, node: &Map<String, Value>) -> Result<EntityRow> {
    let mut attributes = node.clone();
    let embedding = match attributes.remove("embedding") {
        None | Some(Value::Null) => None,
        Some(Value::Array(values)) => Some(encode(
            &values
                .iter()
                .map(Value::as_f64)
                .collect::<Option<Vec<f64>>>()
                .ok_or_else(|| {
                    StoreError::Unstorable(format!(
                        "entity {id}: the embedding is not a list of numbers"
                    ))
                })?,
        )),
        Some(_) => {
            return Err(StoreError::Unstorable(format!(
                "entity {id}: the embedding is not a list"
            )));
        }
    };
    let attributes = to_text(&Value::Object(attributes))?;
    // A hash describes a vector; without one it has nothing to say.
    let embedding_hash = embedding
        .as_ref()
        .and_then(|_| graph.embedding_hash(id))
        .map(str::to_owned);
    Ok(EntityRow {
        hash: hash(&attributes, embedding.as_deref(), embedding_hash.as_deref()),
        id: id.to_owned(),
        attributes,
        embedding,
        embedding_hash,
    })
}

/// Refuse a graph with an embedding that is not a list of numbers, before
/// anything is written.
fn check_rows(graph: &Graph) -> Result<()> {
    for (id, node) in graph.nodes() {
        match node.get("embedding") {
            None | Some(Value::Null) => {}
            Some(Value::Array(values)) if values.iter().all(Value::is_number) => {}
            Some(Value::Array(_)) => {
                return Err(StoreError::Unstorable(format!(
                    "entity {id}: the embedding is not a list of numbers"
                )));
            }
            Some(_) => {
                return Err(StoreError::Unstorable(format!(
                    "entity {id}: the embedding is not a list"
                )));
            }
        }
    }
    Ok(())
}

fn relation_row(
    source: &str,
    target: &str,
    attributes: &Map<String, Value>,
) -> Result<RelationRow> {
    let attributes = to_text(&Value::Object(attributes.clone()))?;
    Ok(RelationRow {
        hash: hash(&attributes, None, None),
        source: source.to_owned(),
        target: target.to_owned(),
        attributes,
    })
}

fn count(changed: usize) -> u64 {
    u64::try_from(changed).unwrap_or(u64::MAX)
}

fn next_revision(conn: &Connection) -> Result<i64> {
    let current: String = conn.query_row(
        "SELECT value FROM meta WHERE key = 'revision_seq'",
        [],
        |row| row.get(0),
    )?;
    let next = current
        .parse::<i64>()
        .map_err(|_| StoreError::Damaged("the revision counter is not a number".to_owned()))?
        + 1;
    conn.execute(
        "UPDATE meta SET value = ?1 WHERE key = 'revision_seq'",
        [next.to_string()],
    )?;
    Ok(next)
}

/// Write the entities that changed; returns the rows written.
fn write_entities(conn: &Connection, key: GraphKey, graph: &Graph) -> Result<u64> {
    let mut old: HashMap<String, (i64, Vec<u8>)> = HashMap::new();
    {
        let mut statement = conn.prepare(
            "SELECT entity_id, ordinal, attr_hash FROM entities
              WHERE project_id = ?1 AND application_id = ?2",
        )?;
        let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
            Ok((row.get::<_, String>(0)?, row.get(1)?, row.get(2)?))
        })?;
        for row in rows {
            let (id, ordinal, hash) = row?;
            old.insert(id, (ordinal, hash));
        }
    }
    let mut written = 0;
    let keep: HashSet<&str> = graph.nodes().map(|(id, _)| id).collect();
    {
        let mut delete = conn.prepare(
            "DELETE FROM entities WHERE project_id = ?1 AND application_id = ?2 AND entity_id = ?3",
        )?;
        for id in old.keys().filter(|id| !keep.contains(id.as_str())) {
            written += count(delete.execute(params![key.project_id, key.application_id, id])?);
        }
    }
    let mut next = old
        .values()
        .map(|(ordinal, _)| *ordinal)
        .max()
        .unwrap_or(-1);
    let mut last = i64::MIN;
    let mut update = conn.prepare(
        "UPDATE entities SET attributes = ?4, embedding = ?5, attr_hash = ?6, embedding_hash = ?7
          WHERE project_id = ?1 AND application_id = ?2 AND entity_id = ?3",
    )?;
    let mut place = conn.prepare(
        "INSERT INTO entities (project_id, application_id, entity_id, ordinal, attributes, embedding,
                               attr_hash, embedding_hash)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
         ON CONFLICT (project_id, application_id, entity_id) DO UPDATE SET
             ordinal = excluded.ordinal, attributes = excluded.attributes,
             embedding = excluded.embedding, attr_hash = excluded.attr_hash,
             embedding_hash = excluded.embedding_hash",
    )?;
    for (id, node) in graph.nodes() {
        let row = entity_row(graph, id, node)?;
        match old.get(&row.id) {
            // Still in order: keeps its ordinal; written only if it changed.
            Some((ordinal, stored)) if *ordinal > last => {
                last = *ordinal;
                if *stored != row.hash {
                    written += count(update.execute(params![
                        key.project_id,
                        key.application_id,
                        row.id,
                        row.attributes,
                        row.embedding,
                        row.hash,
                        row.embedding_hash
                    ])?);
                }
            }
            // New, or moved to the end (removed and added again): the next
            // ordinal, above every stored one.
            _ => {
                next += 1;
                last = next;
                written += count(place.execute(params![
                    key.project_id,
                    key.application_id,
                    row.id,
                    next,
                    row.attributes,
                    row.embedding,
                    row.hash,
                    row.embedding_hash
                ])?);
            }
        }
    }
    Ok(written)
}

/// Write the relations that changed, ordered within their source; returns
/// the rows written.
fn write_relations(conn: &Connection, key: GraphKey, graph: &Graph) -> Result<u64> {
    let mut old: HashMap<(String, String), (i64, Vec<u8>)> = HashMap::new();
    {
        let mut statement = conn.prepare(
            "SELECT source_id, target_id, ordinal, attr_hash FROM relations
              WHERE project_id = ?1 AND application_id = ?2",
        )?;
        let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get(2)?,
                row.get(3)?,
            ))
        })?;
        for row in rows {
            let (source, target, ordinal, hash) = row?;
            old.insert((source, target), (ordinal, hash));
        }
    }
    let mut written = 0;
    let keep: HashSet<(&str, &str)> = graph
        .edges()
        .map(|(source, target, _)| (source, target))
        .collect();
    {
        let mut delete = conn.prepare(
            "DELETE FROM relations WHERE project_id = ?1 AND application_id = ?2
                AND source_id = ?3 AND target_id = ?4",
        )?;
        for (source, target) in old
            .keys()
            .filter(|(source, target)| !keep.contains(&(source.as_str(), target.as_str())))
        {
            written += count(delete.execute(params![
                key.project_id,
                key.application_id,
                source,
                target
            ])?);
        }
    }
    let mut next = old
        .values()
        .map(|(ordinal, _)| *ordinal)
        .max()
        .unwrap_or(-1);
    let mut update = conn.prepare(
        "UPDATE relations SET attributes = ?5, attr_hash = ?6
          WHERE project_id = ?1 AND application_id = ?2 AND source_id = ?3 AND target_id = ?4",
    )?;
    let mut place = conn.prepare(
        "INSERT INTO relations (project_id, application_id, source_id, target_id, ordinal, attributes, attr_hash)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
         ON CONFLICT (project_id, application_id, source_id, target_id) DO UPDATE SET
             ordinal = excluded.ordinal, attributes = excluded.attributes,
             attr_hash = excluded.attr_hash",
    )?;
    // A source's edges come together (Graph::edges walks sources in node
    // order); only the order within a source is the graph's.
    let mut current: Option<&str> = None;
    let mut last = i64::MIN;
    for (source, target, attributes) in graph.edges() {
        if current != Some(source) {
            current = Some(source);
            last = i64::MIN;
        }
        let row = relation_row(source, target, attributes)?;
        match old.get(&(row.source.clone(), row.target.clone())) {
            Some((ordinal, stored)) if *ordinal > last => {
                last = *ordinal;
                if *stored != row.hash {
                    written += count(update.execute(params![
                        key.project_id,
                        key.application_id,
                        row.source,
                        row.target,
                        row.attributes,
                        row.hash
                    ])?);
                }
            }
            _ => {
                next += 1;
                last = next;
                written += count(place.execute(params![
                    key.project_id,
                    key.application_id,
                    row.source,
                    row.target,
                    next,
                    row.attributes,
                    row.hash
                ])?);
            }
        }
    }
    Ok(written)
}

/// Replace the source's documents with `completion`'s, writing only the
/// rows that changed.
fn write_documents(
    conn: &Connection,
    key: GraphKey,
    completion: &Completion<'_>,
    stats: &Stats,
) -> Result<u64> {
    type Stored = (String, String, String, i64, i64);
    let mut old: HashMap<String, Stored> = HashMap::new();
    {
        let mut statement = conn.prepare(
            "SELECT key, version, mime, acl, size, mtime_ns FROM documents
              WHERE project_id = ?1 AND application_id = ?2 AND source_name = ?3",
        )?;
        let rows = statement.query_map(
            params![key.project_id, key.application_id, completion.source_name],
            |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    (
                        row.get(1)?,
                        row.get(2)?,
                        row.get(3)?,
                        row.get(4)?,
                        row.get(5)?,
                    ),
                ))
            },
        )?;
        for row in rows {
            let (document, stored) = row?;
            old.insert(document, stored);
        }
    }
    let mut written = 0;
    {
        let mut delete = conn.prepare(
            "DELETE FROM documents WHERE project_id = ?1 AND application_id = ?2
                AND source_name = ?3 AND key = ?4",
        )?;
        for document in old
            .keys()
            .filter(|document| !completion.documents.contains_key(*document))
        {
            written += count(delete.execute(params![
                key.project_id,
                key.application_id,
                completion.source_name,
                document
            ])?);
        }
    }
    let mut upsert = conn.prepare(
        "INSERT INTO documents (project_id, application_id, source_name, key, version, mime, acl,
                                restricted, size, mtime_ns)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10)
         ON CONFLICT (project_id, application_id, source_name, key) DO UPDATE SET
             version = excluded.version, mime = excluded.mime, acl = excluded.acl,
             restricted = excluded.restricted, size = excluded.size, mtime_ns = excluded.mtime_ns",
    )?;
    for (document, state) in completion.documents {
        let acl = serde_json::to_string(&state.acl)
            .map_err(|error| StoreError::Unstorable(error.to_string()))?;
        let (size, mtime_ns) = stats.get(document).map_or((-1, -1), |(size, mtime)| {
            (i64::try_from(*size).unwrap_or(-1), *mtime)
        });
        let row = (
            state.version.clone(),
            state.mime.clone(),
            acl.clone(),
            size,
            mtime_ns,
        );
        if old.get(document) == Some(&row) {
            continue;
        }
        written += count(upsert.execute(params![
            key.project_id,
            key.application_id,
            completion.source_name,
            document,
            state.version,
            state.mime,
            acl,
            state.acl.is_restricted(),
            size,
            mtime_ns
        ])?);
    }
    Ok(written)
}

/// The rows a commit writes beside the graph's head.
struct Written<'a> {
    stats: &'a Stats,
    policy: Option<&'a str>,
    chunks: Option<ChunkUpdate<'a>>,
}

#[allow(clippy::unnecessary_wraps)] // a row mapper: the outer error is SQLite's
fn stored_chunk(row: &rusqlite::Row<'_>) -> rusqlite::Result<Result<StoredChunk>> {
    let kind: String = row.get(4)?;
    let Some(kind) = ChunkKind::parse(&kind) else {
        return Ok(Err(StoreError::Damaged(format!("a chunk of kind {kind}"))));
    };
    Ok(Ok(StoredChunk {
        id: row.get(0)?,
        path: row.get(1)?,
        line_start: row.get(2)?,
        line_end: row.get(3)?,
        kind,
        text: row.get(5)?,
        text_hash: row.get(6)?,
    }))
}

/// Delete one document's chunks and their FTS rows.
fn delete_chunks_of(conn: &Connection, path: &str) -> Result<u64> {
    let ids: Vec<i64> = {
        let mut statement = conn.prepare_cached("SELECT id FROM chunks WHERE path = ?1")?;
        let rows = statement.query_map([path], |row| row.get(0))?;
        rows.collect::<rusqlite::Result<_>>()?
    };
    let mut fts = conn.prepare_cached("DELETE FROM chunks_fts WHERE rowid = ?1")?;
    for id in &ids {
        fts.execute([id])?;
    }
    Ok(count(
        conn.execute("DELETE FROM chunks WHERE path = ?1", [path])?,
    ))
}

/// Every chunk gone (a removal of the graph or its documents).
fn clear_chunks(conn: &Connection) -> Result<()> {
    conn.execute_batch(
        "DELETE FROM chunks; INSERT INTO chunks_fts (chunks_fts) VALUES ('delete-all');
         DELETE FROM meta WHERE key = 'chunks_generation';",
    )?;
    Ok(())
}

/// The chunks of a commit: those of documents no longer listed dropped,
/// and each staged document's replaced, when it is the version committed
/// (a file that changed again after it was parsed keeps none: the next
/// refresh reads it) and not already stored at that version.
fn write_chunks(
    conn: &Connection,
    completion: &Completion<'_>,
    update: ChunkUpdate<'_>,
) -> Result<u64> {
    conn.execute_batch(STAGING)?;
    let generation: Option<String> = conn
        .query_row(
            "SELECT value FROM meta WHERE key = 'chunks_generation'",
            [],
            |row| row.get(0),
        )
        .optional()?;
    let replace = update.replace || generation.as_deref() != Some(update.generation);
    let stored: HashMap<String, String> = {
        let mut statement = conn.prepare("SELECT DISTINCT path, version FROM chunks")?;
        let rows = statement.query_map([], |row| Ok((row.get(0)?, row.get(1)?)))?;
        rows.collect::<rusqlite::Result<_>>()?
    };
    let mut written = 0;
    for path in stored.keys() {
        if !completion.documents.contains_key(path) {
            written += delete_chunks_of(conn, path)?;
        }
    }
    let mut insert = conn.prepare_cached(
        "INSERT INTO chunks (path, version, line_start, line_end, kind, text, text_hash)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
    )?;
    let mut fts =
        conn.prepare_cached("INSERT INTO chunks_fts (rowid, text, idents) VALUES (?1, ?2, ?3)")?;
    // Row by row, in staging order (a document's rows are together): the
    // commit holds one chunk at a time.
    let mut staged = conn.prepare(
        "SELECT path, version, line_start, line_end, kind, text, text_hash, idents
           FROM temp.chunk_staging ORDER BY rowid",
    )?;
    let mut rows = staged.query([])?;
    let mut current: Option<(String, bool)> = None;
    // The documents whose staged rows were taken.
    let mut taken: HashSet<String> = HashSet::new();
    while let Some(row) = rows.next()? {
        let path: String = row.get(0)?;
        let version: String = row.get(1)?;
        if current.as_ref().is_none_or(|(at, _)| *at != path) {
            let committed = completion.documents.get(&path).map(|state| &state.version);
            let again = taken.contains(&path);
            let take = again
                || (committed == Some(&version)
                    && (replace || stored.get(&path) != Some(&version)));
            if take && !again {
                if stored.contains_key(&path) {
                    written += delete_chunks_of(conn, &path)?;
                }
                taken.insert(path.clone());
            }
            current = Some((path.clone(), take));
        }
        if !current.as_ref().is_some_and(|(_, take)| *take) {
            continue;
        }
        let text: String = row.get(5)?;
        insert.execute(params![
            path,
            version,
            row.get::<_, i64>(2)?,
            row.get::<_, i64>(3)?,
            row.get::<_, String>(4)?,
            text,
            row.get::<_, String>(6)?
        ])?;
        let id = conn.last_insert_rowid();
        fts.execute(params![id, text, row.get::<_, String>(7)?])?;
        written += 1;
    }
    drop(rows);
    drop(staged);
    // A document kept at a new version (or read again) with nothing staged
    // (over the size cap, not decodable, no text): its old chunks describe
    // what it no longer is, and go (their vectors with the next prune).
    for (path, version) in &stored {
        let Some(state) = completion.documents.get(path) else {
            continue;
        };
        if !taken.contains(path) && (replace || state.version != *version) {
            written += delete_chunks_of(conn, path)?;
        }
    }
    conn.execute("DELETE FROM temp.chunk_staging", [])?;
    conn.execute(
        "INSERT INTO meta (key, value) VALUES ('chunks_generation', ?1)
         ON CONFLICT (key) DO UPDATE SET value = excluded.value",
        [update.generation],
    )?;
    Ok(written)
}

/// Whether FTS5 refused the `MATCH` expression itself (`fts5: syntax error
/// near …`, an unknown column, an unterminated string): the query finds
/// nothing. Anything else is the store failing.
fn is_fts_syntax_error(error: &rusqlite::Error) -> bool {
    match error {
        rusqlite::Error::SqliteFailure(failure, Some(message)) => {
            failure.code == rusqlite::ErrorCode::Unknown
                && (message.starts_with("fts5:") || message.contains("unterminated string"))
        }
        _ => false,
    }
}

fn policy_of(conn: &Connection) -> Result<Option<String>> {
    Ok(conn
        .query_row("SELECT value FROM meta WHERE key = 'policy'", [], |row| {
            row.get(0)
        })
        .optional()?)
}

fn last_completed_run(conn: &Connection, key: GraphKey) -> Result<Option<String>> {
    Ok(conn.query_row(
        "SELECT max(finished_at) FROM runs
          WHERE project_id = ?1 AND application_id = ?2 AND status = 'completed'",
        params![key.project_id, key.application_id],
        |row| row.get(0),
    )?)
}

/// The graph's rows and head in `transaction`: the entities and relations
/// that changed, the policy fingerprint when there is one, and the head row
/// at a new revision. Returns the revision and the rows written.
fn write_graph(
    transaction: &rusqlite::Transaction<'_>,
    key: GraphKey,
    graph: &Graph,
    policy: Option<&str>,
) -> Result<(i64, u64)> {
    let mut written = write_entities(transaction, key, graph)?;
    written += write_relations(transaction, key, graph)?;
    if let Some(policy) = policy {
        written += count(transaction.execute(
            "INSERT INTO meta (key, value) VALUES ('policy', ?1)
             ON CONFLICT (key) DO UPDATE SET value = excluded.value",
            [policy],
        )?);
    }
    let revision = next_revision(transaction)?;
    let schema = graph.schema.as_ref().map(to_text).transpose()?;
    written += count(transaction.execute(
        "INSERT INTO graphs (project_id, application_id, revision, attributes, metadata, schema)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6)
         ON CONFLICT (project_id, application_id) DO UPDATE SET
             revision = excluded.revision, attributes = excluded.attributes,
             metadata = excluded.metadata, schema = excluded.schema",
        params![
            key.project_id,
            key.application_id,
            revision,
            to_text(&Value::Object(graph.attributes.clone()))?,
            to_text(&Value::Object(graph.metadata.clone()))?,
            schema
        ],
    )?);
    Ok((revision, written))
}

fn commit(
    conn: &mut Connection,
    key: GraphKey,
    graph: &Graph,
    completion: &Completion<'_>,
    rows: &Written<'_>,
) -> Result<(i64, u64)> {
    let transaction = conn.transaction_with_behavior(TransactionBehavior::Immediate)?;
    let (revision, mut written) = write_graph(&transaction, key, graph, rows.policy)?;
    written += write_documents(&transaction, key, completion, rows.stats)?;
    if let Some(update) = rows.chunks {
        written += write_chunks(&transaction, completion, update)?;
    }
    written += count(transaction.execute(
        &format!(
            "UPDATE sources
                SET status = 'completed', entities_count = ?4, relations_count = ?5,
                    documents_processed = ?6, error_message = NULL, progress_message = NULL,
                    commit_sha = ?7, last_updated = {NOW}
              WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3"
        ),
        params![
            key.project_id,
            key.application_id,
            completion.toolkit_id,
            completion.counts.entities,
            completion.counts.relations,
            completion.counts.documents,
            completion.commit_sha
        ],
    )?);
    let counts = json!({
        "entities": completion.counts.entities,
        "relations": completion.counts.relations,
        "documents": completion.counts.documents,
    })
    .to_string();
    written += count(transaction.execute(
        &format!(
            "UPDATE runs SET status = 'completed', finished_at = {NOW}, counts = ?4
              WHERE id = (SELECT max(id) FROM runs WHERE project_id = ?1 AND application_id = ?2
                            AND toolkit_id = ?3 AND status = 'in_progress')"
        ),
        params![
            key.project_id,
            key.application_id,
            completion.toolkit_id,
            counts
        ],
    )?);
    transaction.commit()?;
    Ok((revision, written))
}

fn parse_map(text: &str, what: &str) -> Result<Map<String, Value>> {
    match serde_json::from_str(text) {
        Ok(Value::Object(map)) => Ok(map),
        _ => Err(StoreError::Damaged(format!("{what} is not a JSON object"))),
    }
}

fn load_read(conn: &mut Connection, key: GraphKey, vectors: bool) -> Result<Option<GraphRead>> {
    let transaction = conn.transaction()?;
    let head: Option<(i64, String, String, Option<String>)> = transaction
        .query_row(
            "SELECT revision, attributes, metadata, schema FROM graphs
              WHERE project_id = ?1 AND application_id = ?2",
            params![key.project_id, key.application_id],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
        )
        .optional()?;
    let Some((revision, attributes, metadata, schema)) = head else {
        return Ok(None);
    };
    let mut graph = Graph::new();
    graph.attributes = parse_map(&attributes, "the graph's attributes")?;
    graph.metadata = parse_map(&metadata, "the graph's metadata")?;
    graph.schema = schema
        .map(|text| {
            serde_json::from_str(&text)
                .map_err(|_| StoreError::Damaged("the graph's schema is not JSON".to_owned()))
        })
        .transpose()?;
    let mut embedded = Vec::new();
    {
        // The blob is selected only when the vectors are wanted; the view
        // asks the database whether there is one.
        let mut statement = transaction.prepare(if vectors {
            "SELECT entity_id, attributes, embedding,
                    (embedding IS NOT NULL AND length(embedding) > 0), embedding_hash
               FROM entities
              WHERE project_id = ?1 AND application_id = ?2 ORDER BY ordinal"
        } else {
            "SELECT entity_id, attributes, NULL,
                    (embedding IS NOT NULL AND length(embedding) > 0), NULL FROM entities
              WHERE project_id = ?1 AND application_id = ?2 ORDER BY ordinal"
        })?;
        let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, Option<Vec<u8>>>(2)?,
                row.get::<_, bool>(3)?,
                row.get::<_, Option<String>>(4)?,
            ))
        })?;
        for row in rows {
            let (id, attributes, embedding, has_vector, embedding_hash) = row?;
            if let Some(embedding_hash) = embedding_hash {
                graph.set_embedding_hash(&id, embedding_hash);
            }
            let mut attributes = parse_map(&attributes, "an entity")?;
            if let Some(embedding) = embedding {
                attributes.insert("embedding".to_owned(), Value::from(decode(&embedding)));
            }
            if has_vector {
                embedded.push(id.clone());
            }
            graph.insert_node(id, attributes);
        }
    }
    {
        let mut statement = transaction.prepare(
            "SELECT source_id, target_id, attributes FROM relations
              WHERE project_id = ?1 AND application_id = ?2 ORDER BY ordinal",
        )?;
        let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, String>(2)?,
            ))
        })?;
        for row in rows {
            let (source, target, attributes) = row?;
            graph.insert_edge(&source, &target, parse_map(&attributes, "a relation")?);
        }
    }
    transaction.commit()?;
    Ok(Some(GraphRead {
        graph,
        revision,
        // Every write draws a revision from one counter, which never
        // repeats, so a revision alone identifies a state of any graph.
        incarnation: String::new(),
        embedded,
    }))
}

fn rank(conn: &Connection, key: GraphKey, vector: &[f64], min_score: f64) -> Result<Ranking> {
    if vector.iter().all(|value| *value == 0.0) {
        return Ok(Ok(Vec::new()));
    }
    let width = vector.len();
    let mut statement = conn.prepare(
        "SELECT entity_id, embedding FROM entities
          WHERE project_id = ?1 AND application_id = ?2
            AND embedding IS NOT NULL AND length(embedding) > 0",
    )?;
    let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
        Ok((row.get::<_, String>(0)?, row.get::<_, Vec<u8>>(1)?))
    })?;
    let mut stored = Vec::new();
    for row in rows {
        let (id, bytes) = row?;
        let embedding = decode(&bytes);
        if embedding.len() != width {
            let other = embedding.len();
            return Ok(Err(format!(
                "shapes ({width},) and ({other},) not aligned: {width} (dim 0) != {other} (dim 0)"
            )));
        }
        stored.push((id, embedding));
    }
    let norm = |values: &[f64]| values.iter().map(|v| v * v).sum::<f64>().sqrt();
    let query_norm = norm(vector);
    let mut ranked: Vec<(String, f64)> = stored
        .into_iter()
        .filter_map(|(id, embedding)| {
            let length = norm(&embedding);
            if length == 0.0 {
                return None;
            }
            let dot: f64 = embedding.iter().zip(vector).map(|(a, b)| a * b).sum();
            let score = dot / (length * query_norm);
            (score >= min_score).then_some((id, score))
        })
        .collect();
    ranked.sort_by(|a, b| b.1.total_cmp(&a.1).then_with(|| a.0.cmp(&b.0)));
    Ok(Ok(ranked))
}

fn status_document(conn: &Connection, key: GraphKey) -> Result<Value> {
    let mut statement = conn.prepare(
        "SELECT toolkit_id, toolkit_name, toolkit_type, status, started_at, last_updated,
                entities_count, relations_count, documents_processed,
                error_message, progress_message, branch
           FROM sources WHERE project_id = ?1 AND application_id = ?2 ORDER BY toolkit_id",
    )?;
    let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
        let toolkit_id: String = row.get(0)?;
        let updated: String = row.get(5)?;
        Ok((
            toolkit_id.clone(),
            updated.clone(),
            json!({
                "toolkit_id": toolkit_id,
                "toolkit_name": row.get::<_, String>(1)?,
                "toolkit_type": row.get::<_, String>(2)?,
                "status": row.get::<_, String>(3)?,
                "started_at": row.get::<_, Option<String>>(4)?,
                "last_updated": updated,
                "entities_count": row.get::<_, i64>(6)?,
                "relations_count": row.get::<_, i64>(7)?,
                "documents_processed": row.get::<_, i64>(8)?,
                "error_message": row.get::<_, Option<String>>(9)?,
                "progress_message": row.get::<_, Option<String>>(10)?,
                "branch": row.get::<_, Option<String>>(11)?,
            }),
        ))
    })?;
    let mut sources = Map::new();
    let mut last_modified: Option<String> = None;
    for row in rows {
        let (toolkit_id, updated, status) = row?;
        if last_modified.as_ref().is_none_or(|known| *known < updated) {
            last_modified = Some(updated);
        }
        sources.insert(toolkit_id, status);
    }
    Ok(json!({"sources": sources, "last_modified": last_modified}))
}

/// Every call but [`GraphStore::lease`] (a set in memory) runs on the
/// blocking pool (`SqliteGraphStore::blocking`), with what it borrows
/// copied: the connection mutex orders them as before, and each write is
/// still one transaction, committed whole even when its future is dropped
/// while it runs.
impl GraphStore for SqliteGraphStore {
    type Error = StoreError;
    type Lease = Lease;

    fn lease(&self, key: GraphKey) -> impl Future<Output = Result<Option<Lease>>> + Send {
        // No I/O: taken when polled, as an async fn would.
        let leases = Arc::clone(&self.leases);
        async move {
            let mut held = leases.lock().unwrap_or_else(PoisonError::into_inner);
            if !held.insert(key) {
                return Ok(None);
            }
            Ok(Some(Lease {
                held: Arc::clone(&leases),
                key,
            }))
        }
    }

    async fn load(&self, key: GraphKey) -> Result<Option<(Graph, i64)>> {
        self.blocking(move |store| store.load_now(key)).await
    }

    async fn load_view(&self, key: GraphKey) -> Result<Option<GraphRead>> {
        self.blocking(move |store| store.load_view_now(key)).await
    }

    async fn revision(&self, key: GraphKey) -> Result<Option<i64>> {
        self.blocking(move |store| store.revision_now(key)).await
    }

    async fn document_versions(
        &self,
        key: GraphKey,
        source_name: &str,
    ) -> Result<BTreeMap<String, String>> {
        let source_name = source_name.to_owned();
        self.blocking(move |store| {
            store.with(|conn| {
                let mut statement = conn.prepare(
                    "SELECT key, version FROM documents
                  WHERE project_id = ?1 AND application_id = ?2 AND source_name = ?3",
                )?;
                let rows = statement.query_map(
                    params![key.project_id, key.application_id, source_name],
                    |row| Ok((row.get(0)?, row.get(1)?)),
                )?;
                Ok(rows.collect::<rusqlite::Result<_>>()?)
            })
        })
        .await
    }

    async fn restricted_documents(&self, key: GraphKey) -> Result<Vec<(String, String, Acl)>> {
        self.blocking(move |store| {
            store.with(|conn| {
                let mut statement = conn.prepare(
                    "SELECT source_name, key, acl FROM documents
                  WHERE project_id = ?1 AND application_id = ?2 AND restricted = 1",
                )?;
                let rows =
                    statement.query_map(params![key.project_id, key.application_id], |row| {
                        Ok((
                            row.get::<_, String>(0)?,
                            row.get::<_, String>(1)?,
                            row.get::<_, String>(2)?,
                        ))
                    })?;
                let mut restricted = Vec::new();
                for row in rows {
                    let (source, document, acl) = row?;
                    let acl: Acl = serde_json::from_str(&acl).map_err(|_| {
                        StoreError::Damaged(format!("the readers of {document} do not parse"))
                    })?;
                    restricted.push((source, document, acl));
                }
                Ok(restricted)
            })
        })
        .await
    }

    async fn start(&self, key: GraphKey, status: &SourceStatus) -> Result<()> {
        let status = status.clone();
        self.blocking(move |store| {
            store.with(|conn| {
                let transaction = conn.transaction()?;
                transaction.execute(
                    &format!(
                        "INSERT INTO sources (project_id, application_id, toolkit_id, toolkit_name,
                         toolkit_type, status, started_at, last_updated, documents_processed,
                         error_message, progress_message, branch)
                     VALUES (?1, ?2, ?3, ?4, ?5, 'in_progress', {NOW}, {NOW}, 0, NULL,
                             'Starting ingestion...', ?6)
                     ON CONFLICT (project_id, application_id, toolkit_id) DO UPDATE SET
                         toolkit_name = excluded.toolkit_name, toolkit_type = excluded.toolkit_type,
                         status = 'in_progress', started_at = excluded.started_at,
                         last_updated = excluded.last_updated, documents_processed = 0,
                         error_message = NULL, progress_message = excluded.progress_message,
                         branch = excluded.branch"
                    ),
                    params![
                        key.project_id,
                        key.application_id,
                        status.toolkit_id,
                        status.toolkit_name,
                        status.toolkit_type,
                        status.branch
                    ],
                )?;
                transaction.execute(
                    &format!(
                    "INSERT INTO runs (project_id, application_id, toolkit_id, started_at, status)
                     VALUES (?1, ?2, ?3, {NOW}, 'in_progress')"
                ),
                    params![key.project_id, key.application_id, status.toolkit_id],
                )?;
                transaction.commit()?;
                Ok(())
            })
        })
        .await
    }

    async fn fail(&self, key: GraphKey, toolkit_id: &str, error: &str) -> Result<()> {
        let (toolkit_id, error) = (toolkit_id.to_owned(), error.to_owned());
        self.blocking(move |store| store.with(|conn| {
            let transaction = conn.transaction()?;
            transaction.execute(
                &format!(
                    "UPDATE sources SET status = 'error', error_message = ?4, progress_message = NULL,
                            last_updated = {NOW}
                      WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3"
                ),
                params![key.project_id, key.application_id, toolkit_id, error],
            )?;
            transaction.execute(
                &format!(
                    "UPDATE runs SET status = 'error', finished_at = {NOW}, error = ?4
                      WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3
                        AND status = 'in_progress'"
                ),
                params![key.project_id, key.application_id, toolkit_id, error],
            )?;
            transaction.commit()?;
            Ok(())
        }))
        .await
    }

    async fn complete(
        &self,
        key: GraphKey,
        graph: &Graph,
        completion: &Completion<'_>,
    ) -> Result<i64> {
        let graph = graph.clone();
        let (toolkit_id, source_name) = (
            completion.toolkit_id.to_owned(),
            completion.source_name.to_owned(),
        );
        let documents = completion.documents.clone();
        let counts = completion.counts;
        let commit_sha = completion.commit_sha.map(str::to_owned);
        self.blocking(move |store| {
            let completion = Completion {
                toolkit_id: &toolkit_id,
                source_name: &source_name,
                documents: &documents,
                counts,
                commit_sha: commit_sha.as_deref(),
            };
            store.complete_with_stats(key, &graph, &completion, &Stats::new(), None)
        })
        .await
    }

    async fn status_document(&self, key: GraphKey) -> Result<Value> {
        self.blocking(move |store| store.status_document_now(key))
            .await
    }

    async fn rank(&self, key: GraphKey, vector: &[f64], min_score: f64) -> Result<Ranking> {
        let vector = vector.to_vec();
        self.blocking(move |store| store.with(|conn| rank(conn, key, &vector, min_score)))
            .await
    }

    async fn delete(&self, key: GraphKey) -> Result<bool> {
        self.blocking(move |store| {
            store.with(|conn| {
                let transaction = conn.transaction()?;
                let mut existed = false;
                for table in [
                    "graphs",
                    "entities",
                    "relations",
                    "documents",
                    "sources",
                    "runs",
                ] {
                    let removed = transaction.execute(
                        &format!(
                            "DELETE FROM {table} WHERE project_id = ?1 AND application_id = ?2"
                        ),
                        params![key.project_id, key.application_id],
                    )?;
                    if table == "graphs" {
                        existed = removed > 0;
                    }
                }
                clear_chunks(&transaction)?;
                transaction.commit()?;
                Ok(existed)
            })
        })
        .await
    }

    async fn save(&self, key: GraphKey, graph: &Graph) -> Result<i64> {
        check_rows(graph)?;
        let graph = graph.clone();
        self.blocking(move |store| {
            store.with(|conn| {
                let transaction = conn.transaction_with_behavior(TransactionBehavior::Immediate)?;
                let (revision, _) = write_graph(&transaction, key, &graph, None)?;
                transaction.commit()?;
                Ok(revision)
            })
        })
        .await
    }

    async fn remove_source(
        &self,
        key: GraphKey,
        graph: &Graph,
        toolkit_id: &str,
        source_name: &str,
    ) -> Result<i64> {
        check_rows(graph)?;
        let graph = graph.clone();
        let (toolkit_id, source_name) = (toolkit_id.to_owned(), source_name.to_owned());
        self.blocking(move |store| {
            store.with(|conn| {
                let transaction = conn.transaction_with_behavior(TransactionBehavior::Immediate)?;
                let (revision, _) = write_graph(&transaction, key, &graph, None)?;
                transaction.execute(
                    "DELETE FROM documents
                  WHERE project_id = ?1 AND application_id = ?2 AND source_name = ?3",
                    params![key.project_id, key.application_id, source_name],
                )?;
                transaction.execute(
                    "DELETE FROM sources
                  WHERE project_id = ?1 AND application_id = ?2 AND toolkit_id = ?3",
                    params![key.project_id, key.application_id, toolkit_id],
                )?;
                // The desktop's one source: its documents' chunks go with them.
                clear_chunks(&transaction)?;
                transaction.commit()?;
                Ok(revision)
            })
        })
        .await
    }

    async fn import(&self, key: GraphKey, graph: &Graph, replace_state: bool) -> Result<Imported> {
        check_rows(graph)?;
        let graph = graph.clone();
        self.blocking(move |store| {
            store.with(|conn| {
                let transaction = conn.transaction_with_behavior(TransactionBehavior::Immediate)?;
                let mut counts = [0_i64; 2];
                for (found, table) in counts.iter_mut().zip(["sources", "documents"]) {
                    *found = transaction.query_row(
                    &format!(
                        "SELECT count(*) FROM {table} WHERE project_id = ?1 AND application_id = ?2"
                    ),
                    params![key.project_id, key.application_id],
                    |row| row.get(0),
                )?;
                    if replace_state {
                        transaction.execute(
                            &format!(
                                "DELETE FROM {table} WHERE project_id = ?1 AND application_id = ?2"
                            ),
                            params![key.project_id, key.application_id],
                        )?;
                    }
                }
                if !replace_state && counts.iter().any(|found| *found > 0) {
                    return Ok(Imported::HasIngestionState {
                        sources: counts[0],
                        documents: counts[1],
                    });
                }
                if replace_state {
                    // The run history belongs to the ingestion state being
                    // replaced, as `delete` clears it.
                    transaction.execute(
                        "DELETE FROM runs WHERE project_id = ?1 AND application_id = ?2",
                        params![key.project_id, key.application_id],
                    )?;
                    clear_chunks(&transaction)?;
                }
                let (revision, _) = write_graph(&transaction, key, &graph, None)?;
                transaction.commit()?;
                Ok(Imported::Saved { revision })
            })
        })
        .await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn failure(code: std::ffi::c_int, message: &str) -> rusqlite::Error {
        rusqlite::Error::SqliteFailure(rusqlite::ffi::Error::new(code), Some(message.to_owned()))
    }

    #[test]
    fn only_a_refused_expression_finds_nothing() {
        assert!(is_fts_syntax_error(&failure(
            rusqlite::ffi::SQLITE_ERROR,
            "fts5: syntax error near \"(\""
        )));
        assert!(is_fts_syntax_error(&failure(
            rusqlite::ffi::SQLITE_ERROR,
            "unterminated string"
        )));
        assert!(!is_fts_syntax_error(&failure(
            rusqlite::ffi::SQLITE_BUSY,
            "database is locked"
        )));
        assert!(!is_fts_syntax_error(&failure(
            rusqlite::ffi::SQLITE_CORRUPT,
            "database disk image is malformed"
        )));
        assert!(!is_fts_syntax_error(&failure(
            rusqlite::ffi::SQLITE_ERROR,
            "no such table: chunks_fts"
        )));
    }

    #[test]
    fn a_bad_expression_finds_nothing_and_a_good_one_runs() {
        let dir = tempfile::tempdir().unwrap();
        let store = SqliteGraphStore::open(&dir.path().join("index")).unwrap();
        assert_eq!(
            store.search_fts("\"unterminated", 10).unwrap(),
            [] as [(i64, f64); 0]
        );
        assert_eq!(
            store.search_fts("AND OR (", 10).unwrap(),
            [] as [(i64, f64); 0]
        );
        assert_eq!(
            store.search_fts("\"word\"", 10).unwrap(),
            [] as [(i64, f64); 0]
        );
    }

    /// A store call that waits on SQLite (here: on the connection, held by
    /// another thread for a while) leaves a current-thread runtime free:
    /// a timer started beside it fires first. Inline, the call held the
    /// runtime's one thread and the timer fired only after it.
    #[test]
    fn a_slow_store_call_does_not_hold_the_runtime() {
        let dir = tempfile::tempdir().unwrap();
        let store = SqliteGraphStore::open(&dir.path().join("index")).unwrap();
        let (held, holding) = std::sync::mpsc::channel();
        let conn = Arc::clone(&store.conn);
        let holder = std::thread::spawn(move || {
            let _guard = conn.lock().unwrap_or_else(PoisonError::into_inner);
            held.send(()).unwrap();
            std::thread::sleep(Duration::from_millis(300));
        });
        holding.recv().unwrap();
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
            .unwrap();
        let (stored, store_done, timer_done) = runtime.block_on(async {
            let call = async {
                let revision = store.revision(GraphKey::LOCAL).await;
                (revision, std::time::Instant::now())
            };
            let timer = async {
                tokio::time::sleep(Duration::from_millis(10)).await;
                std::time::Instant::now()
            };
            let ((revision, store_done), timer_done) = tokio::join!(call, timer);
            (revision, store_done, timer_done)
        });
        holder.join().unwrap();
        assert_eq!(stored.unwrap(), None);
        assert!(
            timer_done < store_done,
            "the timer fired only after the store call returned"
        );
    }
}
