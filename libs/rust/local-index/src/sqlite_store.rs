//! The index's [`GraphStore`]: one owner-only SQLite file per workspace
//! (`<app data>/workspaces/<id>/index/index.sqlite`).
//!
//! Schema version 1 (`PRAGMA user_version`; a newer one is refused, the app
//! that wrote it knows it and this one does not):
//!
//! ```sql
//! meta      (key TEXT PRIMARY KEY, value TEXT)            -- schema_version, revision_seq, setting:*,
//!                                                           -- policy (the fingerprint of the last build's policy)
//! graphs    (project_id, application_id, revision, attributes, metadata, schema)
//! entities  (project_id, application_id, entity_id, ordinal UNIQUE per graph,
//!            attributes, embedding BLOB NULL, attr_hash)
//! relations (project_id, application_id, source_id, target_id, ordinal, attributes, attr_hash)
//! documents (project_id, application_id, source_name, key, version, mime, acl, restricted,
//!            size, mtime_ns)                                -- size/mtime: the folder's stat cache
//! sources   (project_id, application_id, toolkit_id, ... the status document's fields)
//! runs      (id, project_id, application_id, toolkit_id, started_at, finished_at, status,
//!            counts, error, tokens)
//! ```
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
//! store's futures do their SQLite work synchronously: run them on the
//! blocking pool, as the index service does.
//!
//! Embeddings are kept beside the attributes, as the PostgreSQL store keeps
//! them, so [`GraphStore::rank`] is a brute-force cosine scan over them.

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
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

/// `PRAGMA user_version` of the schema this build writes.
pub const SCHEMA_VERSION: i64 = 1;
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
    attributes TEXT NOT NULL, embedding BLOB, attr_hash BLOB NOT NULL,
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
}

/// One row of the `runs` table: its status and when it finished.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RunRow {
    pub status: String,
    pub finished_at: Option<String>,
    pub error: Option<String>,
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
#[derive(Debug)]
pub struct SqliteGraphStore {
    path: PathBuf,
    conn: Mutex<Option<Connection>>,
    leases: Arc<Mutex<HashSet<GraphKey>>>,
    last_commit_rows: AtomicU64,
    /// How many times a graph was loaded from the database.
    loads: AtomicU64,
}

fn open_connection(path: &Path) -> Result<Connection> {
    let conn =
        Connection::open_with_flags(path, OpenFlags::default() | OpenFlags::SQLITE_OPEN_NOFOLLOW)?;
    conn.busy_timeout(Duration::from_secs(5))?;
    conn.pragma_update(None, "journal_mode", "WAL")?;
    conn.pragma_update(None, "synchronous", "NORMAL")?;
    Ok(conn)
}

fn migrate(conn: &Connection) -> Result<()> {
    let version: i64 = conn.query_row("PRAGMA user_version", [], |row| row.get(0))?;
    match version {
        0 => {
            conn.execute_batch(&format!(
                "BEGIN; {SCHEMA}
                 INSERT INTO meta (key, value) VALUES ('schema_version', '{SCHEMA_VERSION}');
                 INSERT INTO meta (key, value) VALUES ('revision_seq', '0');
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
            conn: Mutex::new(Some(conn)),
            leases: Arc::default(),
            last_commit_rows: AtomicU64::new(0),
            loads: AtomicU64::new(0),
        })
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
                  WHERE project_id = ?1 AND application_id = ?2 ORDER BY id",
            )?;
            let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
                Ok(RunRow {
                    status: row.get(0)?,
                    finished_at: row.get(1)?,
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

    fn with<T>(&self, work: impl FnOnce(&mut Connection) -> Result<T>) -> Result<T> {
        let mut guard = self.conn.lock().unwrap_or_else(PoisonError::into_inner);
        let conn = guard.as_mut().ok_or(StoreError::Closed)?;
        work(conn)
    }

    /// A stored setting.
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn setting(&self, name: &str) -> Result<Option<String>> {
        self.with(|conn| {
            Ok(conn
                .query_row(
                    "SELECT value FROM meta WHERE key = ?1",
                    [format!("setting:{name}")],
                    |row| row.get(0),
                )
                .optional()?)
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
        let entities = entity_rows(graph)?;
        let relations = relation_rows(graph)?;
        let written = Written {
            entities: &entities,
            relations: &relations,
            stats,
            policy,
        };
        let (revision, rows) = self.with(|conn| commit(conn, key, graph, completion, &written))?;
        self.last_commit_rows.store(rows, Ordering::SeqCst);
        Ok(revision)
    }
}

// ----------------------------------------------------------------- rows

struct EntityRow {
    id: String,
    attributes: String,
    embedding: Option<Vec<u8>>,
    hash: Vec<u8>,
}

struct RelationRow {
    source: String,
    target: String,
    attributes: String,
    hash: Vec<u8>,
}

fn hash(attributes: &str, embedding: Option<&[u8]>) -> Vec<u8> {
    let mut digest = Sha256::new();
    digest.update(attributes.as_bytes());
    if let Some(embedding) = embedding {
        digest.update([0xff]);
        digest.update(embedding);
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
        .chunks_exact(8)
        .map(|chunk| {
            let mut word = [0_u8; 8];
            word.copy_from_slice(chunk);
            f64::from_le_bytes(word)
        })
        .collect()
}

/// Each node's row, the embedding split off its attributes. Refuses a
/// graph with an embedding that is not a list of numbers, before anything
/// is written.
fn entity_rows(graph: &Graph) -> Result<Vec<EntityRow>> {
    let mut rows = Vec::with_capacity(graph.node_count());
    for (id, node) in graph.nodes() {
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
        rows.push(EntityRow {
            hash: hash(&attributes, embedding.as_deref()),
            id: id.to_owned(),
            attributes,
            embedding,
        });
    }
    Ok(rows)
}

fn relation_rows(graph: &Graph) -> Result<Vec<RelationRow>> {
    let mut rows = Vec::with_capacity(graph.edge_count());
    for (source, target, attributes) in graph.edges() {
        let attributes = to_text(&Value::Object(attributes.clone()))?;
        rows.push(RelationRow {
            hash: hash(&attributes, None),
            source: source.to_owned(),
            target: target.to_owned(),
            attributes,
        });
    }
    Ok(rows)
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
fn write_entities(conn: &Connection, key: GraphKey, entities: &[EntityRow]) -> Result<u64> {
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
    let keep: HashSet<&str> = entities.iter().map(|row| row.id.as_str()).collect();
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
        "UPDATE entities SET attributes = ?4, embedding = ?5, attr_hash = ?6
          WHERE project_id = ?1 AND application_id = ?2 AND entity_id = ?3",
    )?;
    let mut place = conn.prepare(
        "INSERT INTO entities (project_id, application_id, entity_id, ordinal, attributes, embedding, attr_hash)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
         ON CONFLICT (project_id, application_id, entity_id) DO UPDATE SET
             ordinal = excluded.ordinal, attributes = excluded.attributes,
             embedding = excluded.embedding, attr_hash = excluded.attr_hash",
    )?;
    for row in entities {
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
                        row.hash
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
                    row.hash
                ])?);
            }
        }
    }
    Ok(written)
}

/// Write the relations that changed, ordered within their source; returns
/// the rows written.
fn write_relations(conn: &Connection, key: GraphKey, relations: &[RelationRow]) -> Result<u64> {
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
    let keep: HashSet<(&str, &str)> = relations
        .iter()
        .map(|row| (row.source.as_str(), row.target.as_str()))
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
    for row in relations {
        if current != Some(row.source.as_str()) {
            current = Some(row.source.as_str());
            last = i64::MIN;
        }
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
    entities: &'a [EntityRow],
    relations: &'a [RelationRow],
    stats: &'a Stats,
    policy: Option<&'a str>,
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
    entities: &[EntityRow],
    relations: &[RelationRow],
    policy: Option<&str>,
) -> Result<(i64, u64)> {
    let mut written = write_entities(transaction, key, entities)?;
    written += write_relations(transaction, key, relations)?;
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
    let (revision, mut written) = write_graph(
        &transaction,
        key,
        graph,
        rows.entities,
        rows.relations,
        rows.policy,
    )?;
    written += write_documents(&transaction, key, completion, rows.stats)?;
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
                    (embedding IS NOT NULL AND length(embedding) > 0) FROM entities
              WHERE project_id = ?1 AND application_id = ?2 ORDER BY ordinal"
        } else {
            "SELECT entity_id, attributes, NULL,
                    (embedding IS NOT NULL AND length(embedding) > 0) FROM entities
              WHERE project_id = ?1 AND application_id = ?2 ORDER BY ordinal"
        })?;
        let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, Option<Vec<u8>>>(2)?,
                row.get::<_, bool>(3)?,
            ))
        })?;
        for row in rows {
            let (id, attributes, embedding, has_vector) = row?;
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

impl GraphStore for SqliteGraphStore {
    type Error = StoreError;
    type Lease = Lease;

    async fn lease(&self, key: GraphKey) -> Result<Option<Lease>> {
        let mut held = self.leases.lock().unwrap_or_else(PoisonError::into_inner);
        if !held.insert(key) {
            return Ok(None);
        }
        Ok(Some(Lease {
            held: Arc::clone(&self.leases),
            key,
        }))
    }

    async fn load(&self, key: GraphKey) -> Result<Option<(Graph, i64)>> {
        self.load_now(key)
    }

    async fn load_view(&self, key: GraphKey) -> Result<Option<GraphRead>> {
        self.load_view_now(key)
    }

    async fn revision(&self, key: GraphKey) -> Result<Option<i64>> {
        self.revision_now(key)
    }

    async fn document_versions(
        &self,
        key: GraphKey,
        source_name: &str,
    ) -> Result<BTreeMap<String, String>> {
        self.with(|conn| {
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
    }

    async fn restricted_documents(&self, key: GraphKey) -> Result<Vec<(String, String, Acl)>> {
        self.with(|conn| {
            let mut statement = conn.prepare(
                "SELECT source_name, key, acl FROM documents
                  WHERE project_id = ?1 AND application_id = ?2 AND restricted = 1",
            )?;
            let rows = statement.query_map(params![key.project_id, key.application_id], |row| {
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
    }

    async fn start(&self, key: GraphKey, status: &SourceStatus) -> Result<()> {
        self.with(|conn| {
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
    }

    async fn fail(&self, key: GraphKey, toolkit_id: &str, error: &str) -> Result<()> {
        self.with(|conn| {
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
        })
    }

    async fn complete(
        &self,
        key: GraphKey,
        graph: &Graph,
        completion: &Completion<'_>,
    ) -> Result<i64> {
        self.complete_with_stats(key, graph, completion, &Stats::new(), None)
    }

    async fn status_document(&self, key: GraphKey) -> Result<Value> {
        self.status_document_now(key)
    }

    async fn rank(&self, key: GraphKey, vector: &[f64], min_score: f64) -> Result<Ranking> {
        self.with(|conn| rank(conn, key, vector, min_score))
    }

    async fn delete(&self, key: GraphKey) -> Result<bool> {
        self.with(|conn| {
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
                    &format!("DELETE FROM {table} WHERE project_id = ?1 AND application_id = ?2"),
                    params![key.project_id, key.application_id],
                )?;
                if table == "graphs" {
                    existed = removed > 0;
                }
            }
            transaction.commit()?;
            Ok(existed)
        })
    }

    async fn save(&self, key: GraphKey, graph: &Graph) -> Result<i64> {
        let entities = entity_rows(graph)?;
        let relations = relation_rows(graph)?;
        self.with(|conn| {
            let transaction = conn.transaction_with_behavior(TransactionBehavior::Immediate)?;
            let (revision, _) = write_graph(&transaction, key, graph, &entities, &relations, None)?;
            transaction.commit()?;
            Ok(revision)
        })
    }

    async fn remove_source(
        &self,
        key: GraphKey,
        graph: &Graph,
        toolkit_id: &str,
        source_name: &str,
    ) -> Result<i64> {
        let entities = entity_rows(graph)?;
        let relations = relation_rows(graph)?;
        self.with(|conn| {
            let transaction = conn.transaction_with_behavior(TransactionBehavior::Immediate)?;
            let (revision, _) = write_graph(&transaction, key, graph, &entities, &relations, None)?;
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
            transaction.commit()?;
            Ok(revision)
        })
    }

    async fn import(&self, key: GraphKey, graph: &Graph, replace_state: bool) -> Result<Imported> {
        let entities = entity_rows(graph)?;
        let relations = relation_rows(graph)?;
        self.with(|conn| {
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
            let (revision, _) = write_graph(&transaction, key, graph, &entities, &relations, None)?;
            transaction.commit()?;
            Ok(Imported::Saved { revision })
        })
    }
}
