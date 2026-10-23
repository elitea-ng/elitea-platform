//! The local agent's conversation memory: `sessions.sqlite` in the app data
//! directory (next to `threads.sqlite`), behind the runtime's
//! [`elitea_agent_runtime::host::StateStore`] (ADR-0029, "Desktop: SQLite").
//!
//! A local turn used to start from an empty model context. Now a Local work
//! thread is one adk **session**: every turn reopens it, the runner folds its
//! events (user and assistant messages, tool calls and their results, in
//! order) into the model request, and appends the new turn's events to it.
//! The session outlives the app.
//!
//! **The server conversation stays canonical.** This store is a cache and an
//! accelerator: when it has nothing for a thread (a new device, wiped data,
//! a trimmed head, a take-back that brought the cloud's turns, a refused
//! commit, a looser `path_deny` now gone) the session is rebuilt from the
//! server (`d0/turn.rs` and `seed.rs`). Every path can rebuild.
//!
//! * [`session_key`]: `sha256(deployment origin ‖ user id ‖ conversation
//!   UUID)`. Never the workspace and never the numeric conversation id:
//!   another account or deployment never reads another's session.
//! * On disk an event is the adk [`Event`] as plain `serde_json` (no
//!   `preserve_order`, no D0-local type), so the runtime's own history
//!   projection can adopt the rows unchanged.
//! * Bounds: a tool response is cut at [`MAX_TOOL_RESPONSE_BYTES`] (the cap
//!   the server keeps); a session holds at most [`MAX_SESSION_BYTES`], past
//!   which the oldest WHOLE invocations go (never half of a call/response
//!   pair) and the session is marked for re-seeding. The session state the
//!   dropped events built stays: one event with no content, at the head,
//!   carries it. A re-seed keeps the keys its caller names (the thread's
//!   model choice, `d0/session.rs`).
//! * The session remembers the `path_deny` fingerprint its tool responses
//!   were read under. A different one drops those response bodies (messages
//!   stay) and marks the session for re-seeding without bodies: content read
//!   under a looser policy does not survive the change.
//! * A cancelled turn leaves nothing ([`SessionStore::finish_turn`]); a turn
//!   the server refused marks the session for re-seeding, and a turn that
//!   never finished (a crash) is dropped when the session is next opened.
//!
//! One thread owns the SQLite connection ([`SessionStore`] is an actor):
//! the turn path awaits a reply and never blocks a runtime worker on SQLite,
//! and every read sees every earlier write.
//!
//! Protection, on Unix, exactly as `threads.sqlite`: `0700` directory,
//! database created `0600` before SQLite opens it (it gives `-wal`/`-shm` the
//! same mode), opened without following a symlink, and a file that is a
//! symlink, not regular, another user's or group/world-accessible is refused
//! (the app then runs without it; turns still run, seeded from the server,
//! and a warning is logged). Contents are never logged.
//!
//! Schema (`PRAGMA user_version` = [`SCHEMA_VERSION`], WAL journal):
//!
//! ```sql
//! sessions (session_key TEXT PRIMARY KEY, deployment TEXT, user_id INTEGER,
//!           project_id INTEGER, conversation_uuid TEXT, thread_nonce BLOB,
//!           seeded_from_server INTEGER /* 0|1 */, server_cursor TEXT NULL,
//!           created_at INTEGER, updated_at INTEGER, bytes INTEGER,
//!           policy_fp TEXT /* sha256 of path_deny */)
//! events   (session_key TEXT REFERENCES sessions ON DELETE CASCADE, seq INTEGER,
//!           turn_execution_id TEXT, executed_by TEXT /* desktop|cloud_executor */,
//!           response_message_id TEXT NULL, invocation_id TEXT, author TEXT,
//!           event TEXT /* adk Event JSON */, bytes INTEGER,
//!           PRIMARY KEY (session_key, seq)) WITHOUT ROWID
//! ```
//!
//! `server_cursor` is the conversation's message-group COUNT when the
//! session last agreed with the server (the conversation detail every turn
//! already reads carries it): a later turn that reads a different count
//! (another device or the cloud wrote, or a message was deleted) re-seeds.

use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{Receiver, Sender};
use std::sync::{Arc, Mutex, RwLock};
use std::time::Duration;

use adk_core::{Event, Part};
use adk_graph::{Checkpointer, MemoryCheckpointer};
use adk_session::{
    AppendEventRequest, CreateRequest, DeleteRequest, Events, GetRequest, ListRequest, Session,
    SessionService, State, state_utils,
};
use async_trait::async_trait;
use chrono::{DateTime, TimeZone as _, Utc};
use elitea_agent_runtime::host::StateStore;
use rusqlite::{Connection, OpenFlags, OptionalExtension as _, Transaction, params};
use serde::Serialize;
use serde_json::Value;
use sha2::{Digest as _, Sha256};

use crate::credentials_file::{create_private_dir, create_private_file};
use crate::error::HostError;
use crate::history::{inspect_private, move_files_aside};

pub const FILE_NAME: &str = "sessions.sqlite";
/// `PRAGMA user_version` of the schema this build writes.
pub const SCHEMA_VERSION: i64 = 1;
/// What one session may hold in all (event JSON bytes).
pub const MAX_SESSION_BYTES: i64 = 32 * 1024 * 1024;
/// A tool response is stored cut at this many bytes: the cap the server
/// keeps for a step's output (`d0/recorder.rs` `MAX_TOOL_OUTPUT`).
pub const MAX_TOOL_RESPONSE_BYTES: usize = 64 * 1024;
/// The adk app name and user id of every desktop session (the key, not
/// these, tells sessions apart).
pub const APP_NAME: &str = "elitea-desktop";
pub const ADK_USER: &str = "local";
pub const EXECUTED_BY_DESKTOP: &str = "desktop";
pub const EXECUTED_BY_CLOUD: &str = "cloud_executor";
/// What replaces a tool response whose body must not outlive its policy.
pub const WITHHELD_BODY: &str =
    "[output withheld: the path policy changed since this was read; re-run or re-read]";
const BINARY_NOTE: &str = "[binary content not kept]";

const SCHEMA: &str = "
CREATE TABLE sessions (
    session_key TEXT PRIMARY KEY NOT NULL,
    deployment TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    project_id INTEGER NOT NULL,
    conversation_uuid TEXT NOT NULL,
    thread_nonce BLOB NOT NULL,
    seeded_from_server INTEGER NOT NULL DEFAULT 0,
    server_cursor TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    bytes INTEGER NOT NULL DEFAULT 0,
    policy_fp TEXT NOT NULL DEFAULT ''
);
CREATE TABLE events (
    session_key TEXT NOT NULL REFERENCES sessions (session_key) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    turn_execution_id TEXT NOT NULL,
    executed_by TEXT NOT NULL,
    response_message_id TEXT,
    invocation_id TEXT,
    author TEXT,
    event TEXT NOT NULL,
    bytes INTEGER NOT NULL,
    PRIMARY KEY (session_key, seq)
) WITHOUT ROWID;
CREATE INDEX events_by_turn ON events (session_key, turn_execution_id);
";

fn storage(what: &str, error: &rusqlite::Error) -> HostError {
    HostError::Storage(format!("{what}: {error}"))
}

fn now_ms() -> i64 {
    Utc::now().timestamp_millis()
}

/// Whose session it is: the deployment, the user there, and the
/// conversation. The project is kept for the record, not the key.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SessionIdentity {
    /// The deployment origin, no trailing slash.
    pub deployment: String,
    pub user_id: i64,
    pub project_id: i64,
    /// The conversation's UUID, lowercase.
    pub conversation_uuid: String,
}

impl SessionIdentity {
    #[must_use]
    pub fn key(&self) -> String {
        session_key(&self.deployment, self.user_id, &self.conversation_uuid)
    }
}

/// `sha256(deployment ‖ user id ‖ conversation UUID)` in hex. The parts are
/// separated by a byte none of them holds, so no two triples share a key.
#[must_use]
pub fn session_key(deployment: &str, user_id: i64, conversation_uuid: &str) -> String {
    let mut hash = Sha256::new();
    hash.update(deployment.trim_end_matches('/').as_bytes());
    hash.update([0]);
    hash.update(user_id.to_string().as_bytes());
    hash.update([0]);
    hash.update(conversation_uuid.to_ascii_lowercase().as_bytes());
    format!("{:x}", hash.finalize())
}

/// What a session's tool responses were read under: a digest of `path_deny`
/// (order and repeats do not matter).
#[must_use]
pub fn policy_fingerprint(path_deny: &[String]) -> String {
    let mut sorted: Vec<&str> = path_deny.iter().map(String::as_str).collect();
    sorted.sort_unstable();
    sorted.dedup();
    let mut hash = Sha256::new();
    for pattern in sorted {
        hash.update(pattern.as_bytes());
        hash.update([0]);
    }
    format!("{:x}", hash.finalize())
}

/// One event to store, with what the row records about it.
#[derive(Clone, Debug)]
pub struct NewEvent {
    pub turn_execution_id: String,
    pub executed_by: &'static str,
    pub response_message_id: Option<String>,
    pub event: Event,
}

/// What opening a session found.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SessionState {
    /// The session agrees with the server as of `server_cursor`.
    pub seeded_from_server: bool,
    pub server_cursor: Option<String>,
    pub events: usize,
    /// The `path_deny` fingerprint differed: the tool-response bodies were
    /// dropped and the re-seed must not bring them back.
    pub bodies_dropped: bool,
}

/// `session_info`'s answer.
#[derive(Clone, Debug, Serialize, PartialEq, Eq)]
pub struct SessionInfo {
    pub turns: u64,
    pub bytes: u64,
    pub seeded_from_server: bool,
}

/// How a turn ended, for its session.
#[derive(Clone, Debug)]
pub enum TurnOutcome {
    /// The server saved the turn: its events are the server's now too.
    Committed {
        response_message_id: String,
        /// The conversation's group count after the commit, when known.
        cursor: Option<String>,
    },
    /// Cancelled: nothing of it was saved, so nothing of it stays.
    Cancelled,
    /// The server refused or never confirmed the commit: the events stay,
    /// the session is re-seeded before its next turn.
    Refused,
}

type Job = Box<dyn FnOnce(&mut Connection) + Send + 'static>;

/// The turns running now, by session key: one per session.
#[derive(Default)]
struct Active(Mutex<HashMap<String, String>>);

impl Active {
    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<String, String>> {
        self.0
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

/// A turn's claim on its session; released on drop.
pub struct TurnScope {
    active: Arc<Active>,
    key: String,
    execution_id: String,
}

impl Drop for TurnScope {
    fn drop(&mut self) {
        let mut active = self.active.lock();
        if active.get(&self.key) == Some(&self.execution_id) {
            active.remove(&self.key);
        }
    }
}

impl TurnScope {
    /// The server created the turn: tag its events with the execution's id.
    pub fn retag(&mut self, execution_id: &str) {
        let mut active = self.active.lock();
        if active.get(&self.key) == Some(&self.execution_id) {
            active.insert(self.key.clone(), execution_id.to_owned());
        }
        self.execution_id = execution_id.to_owned();
    }
}

/// The connection's thread and the queue to it.
struct Writing {
    sender: Sender<Job>,
    thread: std::thread::JoinHandle<()>,
}

pub struct SessionStore {
    dir: PathBuf,
    path: PathBuf,
    sender: RwLock<Option<Sender<Job>>>,
    thread: Mutex<Option<std::thread::JoinHandle<()>>>,
    active: Arc<Active>,
}

fn open_connection(path: &Path) -> Result<Connection, HostError> {
    let conn =
        Connection::open_with_flags(path, OpenFlags::default() | OpenFlags::SQLITE_OPEN_NOFOLLOW)
            .map_err(|e| storage("could not open the session store", &e))?;
    conn.busy_timeout(Duration::from_secs(5))
        .map_err(|e| storage("could not configure the session store", &e))?;
    conn.pragma_update(None, "journal_mode", "WAL")
        .map_err(|e| storage("could not configure the session store", &e))?;
    conn.pragma_update(None, "synchronous", "NORMAL")
        .map_err(|e| storage("could not configure the session store", &e))?;
    conn.pragma_update(None, "foreign_keys", "ON")
        .map_err(|e| storage("could not configure the session store", &e))?;
    Ok(conn)
}

fn migrate(conn: &Connection) -> Result<(), HostError> {
    let version: i64 = conn
        .query_row("PRAGMA user_version", [], |row| row.get(0))
        .map_err(|e| storage("could not read the session store", &e))?;
    match version {
        0 => conn
            .execute_batch(&format!(
                "BEGIN; {SCHEMA} PRAGMA user_version = {SCHEMA_VERSION}; COMMIT;"
            ))
            .map_err(|e| storage("could not create the session store", &e)),
        SCHEMA_VERSION => Ok(()),
        newer => Err(HostError::Storage(format!(
            "the session store was written by a newer version of the app (schema {newer})"
        ))),
    }
}

/// Open the database at `path` (created owner-only first) and start the
/// thread that owns its connection.
fn start(path: &Path) -> Result<Writing, HostError> {
    // Created owner-only BEFORE SQLite opens it, so it is never wider.
    match create_private_file(path) {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(e) => return Err(HostError::Storage(e.to_string())),
    }
    for sidecar in ["-wal", "-shm"] {
        inspect_private(
            &path.with_file_name(format!("{FILE_NAME}{sidecar}")),
            "the session store",
        )?;
    }
    let conn = open_connection(path)?;
    migrate(&conn)?;
    let (sender, receiver) = std::sync::mpsc::channel::<Job>();
    let thread = std::thread::Builder::new()
        .name("session-store".into())
        .spawn(move || run_jobs(conn, &receiver))
        .map_err(|e| HostError::Storage(format!("could not start the session store: {e}")))?;
    Ok(Writing { sender, thread })
}

fn run_jobs(mut conn: Connection, receiver: &Receiver<Job>) {
    while let Ok(job) = receiver.recv() {
        job(&mut conn);
    }
    // The queue closed: fold the WAL into the database before the file may
    // be moved or read by another process (the Doctor).
    if let Err(error) = conn.execute_batch("PRAGMA wal_checkpoint(TRUNCATE);") {
        log::warn!("the session store could not be checkpointed: {error}");
    }
}

fn closed() -> HostError {
    HostError::Storage("the session store is closed; restart Elitea".into())
}

impl SessionStore {
    /// Open (or create) `sessions.sqlite` in `dir` and start its thread.
    ///
    /// # Errors
    ///
    /// The directory or file is refused (see the module docs), the file is
    /// not a database, or it was written by a newer schema.
    pub fn open(dir: &Path) -> Result<Self, HostError> {
        create_private_dir(dir)?;
        // Canonical, so the no-follow open refuses a symlinked FILE, not a
        // symlink higher up (macOS's `/var` is one).
        let dir = fs::canonicalize(dir).map_err(|e| HostError::Storage(e.to_string()))?;
        let path = dir.join(FILE_NAME);
        inspect_private(&path, "the session store")?;
        let writing = start(&path)?;
        Ok(Self {
            dir,
            path,
            sender: RwLock::new(Some(writing.sender)),
            thread: Mutex::new(Some(writing.thread)),
            active: Arc::new(Active::default()),
        })
    }

    #[must_use]
    pub fn path(&self) -> &Path {
        &self.path
    }

    fn submit(&self, job: Job) -> Result<(), HostError> {
        let sender = self
            .sender
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        sender
            .as_ref()
            .ok_or_else(closed)?
            .send(job)
            .map_err(|_| closed())
    }

    /// Run `work` on the connection's thread and wait for its answer
    /// without holding a runtime worker.
    async fn call<T: Send + 'static>(
        &self,
        what: &'static str,
        work: impl FnOnce(&mut Connection) -> rusqlite::Result<T> + Send + 'static,
    ) -> Result<T, HostError> {
        let (reply, answer) = tokio::sync::oneshot::channel();
        self.submit(Box::new(move |conn| {
            let _ = reply.send(work(conn).map_err(|e| storage(what, &e)));
        }))?;
        answer.await.map_err(|_| closed())?
    }

    /// [`Self::call`] for a thread that may block (not an async worker).
    fn call_blocking<T: Send + 'static>(
        &self,
        what: &'static str,
        work: impl FnOnce(&mut Connection) -> rusqlite::Result<T> + Send + 'static,
    ) -> Result<T, HostError> {
        let (reply, answer) = std::sync::mpsc::channel();
        self.submit(Box::new(move |conn| {
            let _ = reply.send(work(conn).map_err(|e| storage(what, &e)));
        }))?;
        answer.recv().map_err(|_| closed())?
    }

    /// Stop the thread (its queue drains first) and checkpoint.
    fn close(&self) {
        drop(
            self.sender
                .write()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .take(),
        );
        let thread = self
            .thread
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take();
        if let Some(thread) = thread
            && thread.join().is_err()
        {
            log::warn!("the session store's thread panicked");
        }
    }

    /// The Doctor's repair: close the store, move `sessions.sqlite` with its
    /// `-wal` and `-shm` TOGETHER into a new directory beside it
    /// (`sessions.sqlite.broken-…/`), then start a fresh, empty store. The
    /// directory it moved them to. Sessions are a cache: the conversations
    /// on the server rebuild them.
    ///
    /// # Errors
    ///
    /// The files could not be moved (the store is then reopened on them), or
    /// the fresh store could not be started (it stays closed until restart).
    pub fn move_aside(&self) -> Result<PathBuf, HostError> {
        self.close();
        let moved = move_files_aside(&self.dir, FILE_NAME, &session_files(&self.dir));
        let started = start(&self.path);
        let writing = match (&moved, started) {
            (_, Ok(writing)) => writing,
            (Err(_), Err(_)) => return moved,
            (Ok(aside), Err(error)) => {
                return Err(HostError::Storage(format!(
                    "moved to {}, but a new session store could not start ({error}); restart Elitea",
                    aside.display()
                )));
            }
        };
        *self
            .sender
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(writing.sender);
        *self
            .thread
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(writing.thread);
        let aside = moved?;
        log::warn!(
            "diagnostics: moved the session store aside to {}",
            aside.display()
        );
        Ok(aside)
    }

    /// Open the session of `identity` for a turn: created when missing,
    /// re-keyed to `policy_fp` (a different `path_deny` drops the tool
    /// responses' bodies and marks the session for re-seeding), and cleared
    /// of what an earlier turn left unfinished (a crash). Never touches a
    /// session a running turn holds.
    ///
    /// # Errors
    ///
    /// The store could not be read or written.
    pub async fn open_session(
        &self,
        identity: &SessionIdentity,
        policy_fp: &str,
    ) -> Result<SessionState, HostError> {
        let identity = identity.clone();
        let fp = policy_fp.to_owned();
        let key = identity.key();
        let busy = self.active.lock().contains_key(&key);
        self.call("could not open the session", move |conn| {
            let tx = conn.transaction()?;
            let row: Option<(i64, Option<String>, String)> = tx
                .query_row(
                    "SELECT seeded_from_server, server_cursor, policy_fp
                     FROM sessions WHERE session_key = ?1",
                    params![key],
                    |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
                )
                .optional()?;
            let state = if let Some((seeded, cursor, stored_fp)) = row {
                if !busy {
                    // An earlier turn that never ended (no answer id).
                    tx.execute(
                        "DELETE FROM events
                         WHERE session_key = ?1 AND response_message_id IS NULL",
                        params![key],
                    )?;
                }
                let changed = stored_fp != fp;
                if changed {
                    withhold_bodies(&tx, &key)?;
                    tx.execute(
                        "UPDATE sessions SET policy_fp = ?2, seeded_from_server = 0,
                             server_cursor = NULL WHERE session_key = ?1",
                        params![key, fp],
                    )?;
                }
                refresh_bytes(&tx, &key)?;
                let events: i64 = tx.query_row(
                    "SELECT COUNT(*) FROM events WHERE session_key = ?1",
                    params![key],
                    |row| row.get(0),
                )?;
                SessionState {
                    seeded_from_server: seeded != 0 && !changed,
                    server_cursor: if changed { None } else { cursor },
                    events: usize::try_from(events).unwrap_or_default(),
                    bodies_dropped: changed,
                }
            } else {
                let nonce: [u8; 16] = rand::random();
                tx.execute(
                    "INSERT INTO sessions (session_key, deployment, user_id, project_id,
                         conversation_uuid, thread_nonce, seeded_from_server, created_at,
                         updated_at, policy_fp)
                     VALUES (?1, ?2, ?3, ?4, ?5, ?6, 0, ?7, ?7, ?8)",
                    params![
                        key,
                        identity.deployment,
                        identity.user_id,
                        identity.project_id,
                        identity.conversation_uuid,
                        nonce.to_vec(),
                        now_ms(),
                        fp
                    ],
                )?;
                SessionState {
                    seeded_from_server: false,
                    server_cursor: None,
                    events: 0,
                    bodies_dropped: false,
                }
            };
            tx.commit()?;
            Ok(state)
        })
        .await
    }

    /// Replace the session's events with `events` (the server's transcript,
    /// folded) and mark it seeded as of `cursor`. The session state under
    /// the keys in `carry` (the thread's own settings, which the server's
    /// transcript does not hold) stays as the replaced events left it.
    ///
    /// # Errors
    ///
    /// The store could not be written.
    pub async fn seed(
        &self,
        key: &str,
        events: Vec<NewEvent>,
        cursor: Option<String>,
        carry: &'static [&'static str],
    ) -> Result<(), HostError> {
        let key = key.to_owned();
        self.call("could not seed the session", move |conn| {
            let tx = conn.transaction()?;
            let mut carried = Carried::default();
            if !carry.is_empty() {
                let rows: Vec<String> = tx
                    .prepare("SELECT event FROM events WHERE session_key = ?1 ORDER BY seq")?
                    .query_map(params![key], |row| row.get(0))?
                    .collect::<rusqlite::Result<_>>()?;
                carried.fold(&rows);
                carried
                    .state
                    .retain(|name, _| carry.contains(&name.as_str()));
            }
            tx.execute("DELETE FROM events WHERE session_key = ?1", params![key])?;
            carry_state(&tx, &key, 0, carried)?;
            for (index, new) in events.into_iter().enumerate() {
                insert_event(&tx, &key, i64::try_from(index + 1).unwrap_or(i64::MAX), new)?;
            }
            trim(&tx, &key)?;
            tx.execute(
                "UPDATE sessions SET seeded_from_server = 1, server_cursor = ?2, updated_at = ?3
                 WHERE session_key = ?1",
                params![key, cursor, now_ms()],
            )?;
            refresh_bytes(&tx, &key)?;
            tx.commit()
        })
        .await
    }

    /// Mark the session of this conversation for re-seeding (a take-back
    /// brought the cloud's turns). No session is not an error.
    ///
    /// # Errors
    ///
    /// The store could not be written.
    pub async fn mark_reseed(
        &self,
        deployment: &str,
        user_id: i64,
        conversation_uuid: &str,
    ) -> Result<bool, HostError> {
        let key = session_key(deployment, user_id, conversation_uuid);
        self.call("could not mark the session", move |conn| {
            let changed = conn.execute(
                "UPDATE sessions SET seeded_from_server = 0, server_cursor = NULL
                 WHERE session_key = ?1",
                params![key],
            )?;
            Ok(changed > 0)
        })
        .await
    }

    /// Claim the session for one running turn; its events are tagged with
    /// `execution_id`. Refused while another turn holds the session.
    ///
    /// # Errors
    ///
    /// Another turn of the same conversation is running.
    pub fn begin_turn(&self, key: &str, execution_id: &str) -> Result<TurnScope, HostError> {
        let mut active = self.active.lock();
        if active.contains_key(key) {
            return Err(HostError::Internal(
                "another turn of this conversation is running".into(),
            ));
        }
        active.insert(key.to_owned(), execution_id.to_owned());
        drop(active);
        Ok(TurnScope {
            active: self.active.clone(),
            key: key.to_owned(),
            execution_id: execution_id.to_owned(),
        })
    }

    /// Settle a turn's events: see [`TurnOutcome`].
    ///
    /// # Errors
    ///
    /// The store could not be written.
    pub async fn finish_turn(
        &self,
        key: &str,
        execution_id: &str,
        outcome: TurnOutcome,
    ) -> Result<(), HostError> {
        let key = key.to_owned();
        let execution_id = execution_id.to_owned();
        self.call("could not settle the turn in its session", move |conn| {
            let tx = conn.transaction()?;
            match outcome {
                TurnOutcome::Committed {
                    response_message_id,
                    cursor,
                } => {
                    tx.execute(
                        "UPDATE events SET response_message_id = ?3
                         WHERE session_key = ?1 AND turn_execution_id = ?2",
                        params![key, execution_id, response_message_id],
                    )?;
                    let trimmed = trim(&tx, &key)?;
                    tx.execute(
                        "UPDATE sessions SET server_cursor = ?2, updated_at = ?3,
                             seeded_from_server = CASE WHEN ?4 THEN 0 ELSE seeded_from_server END
                         WHERE session_key = ?1",
                        params![key, cursor, now_ms(), trimmed],
                    )?;
                }
                TurnOutcome::Cancelled => {
                    tx.execute(
                        "DELETE FROM events WHERE session_key = ?1 AND turn_execution_id = ?2",
                        params![key, execution_id],
                    )?;
                }
                TurnOutcome::Refused => {
                    tx.execute(
                        "UPDATE sessions SET seeded_from_server = 0, server_cursor = NULL
                         WHERE session_key = ?1",
                        params![key],
                    )?;
                }
            }
            refresh_bytes(&tx, &key)?;
            tx.commit()
        })
        .await
    }

    /// Forget one conversation's session.
    ///
    /// # Errors
    ///
    /// The store could not be written.
    pub async fn delete_conversation(
        &self,
        deployment: &str,
        user_id: i64,
        conversation_uuid: &str,
    ) -> Result<bool, HostError> {
        let key = session_key(deployment, user_id, conversation_uuid);
        self.call("could not delete the session", move |conn| {
            Ok(conn.execute("DELETE FROM sessions WHERE session_key = ?1", params![key])? > 0)
        })
        .await
    }

    /// Forget every session (a wipe). A blocking call.
    ///
    /// # Errors
    ///
    /// The store could not be written.
    pub fn delete_all(&self) -> Result<usize, HostError> {
        self.call_blocking("could not delete the sessions", |conn| {
            conn.execute("DELETE FROM sessions", [])
        })
    }

    /// What `session_info` answers for a conversation; `None` without one.
    ///
    /// # Errors
    ///
    /// The store could not be read.
    pub async fn info(
        &self,
        deployment: &str,
        user_id: i64,
        conversation_uuid: &str,
    ) -> Result<Option<SessionInfo>, HostError> {
        let key = session_key(deployment, user_id, conversation_uuid);
        self.call("could not read the session", move |conn| {
            conn.query_row(
                "SELECT seeded_from_server, bytes,
                        (SELECT COUNT(DISTINCT turn_execution_id) FROM events e
                         WHERE e.session_key = s.session_key)
                 FROM sessions s WHERE session_key = ?1",
                params![key],
                |row| {
                    Ok(SessionInfo {
                        seeded_from_server: row.get::<_, i64>(0)? != 0,
                        bytes: u64::try_from(row.get::<_, i64>(1)?).unwrap_or_default(),
                        turns: u64::try_from(row.get::<_, i64>(2)?).unwrap_or_default(),
                    })
                },
            )
            .optional()
        })
        .await
    }

    /// The session's stored events, oldest first, with what each row
    /// records (what `session_info` summarises; tests read the rows).
    ///
    /// # Errors
    ///
    /// The store could not be read.
    #[cfg_attr(not(test), allow(dead_code))]
    pub async fn rows(&self, key: &str) -> Result<Vec<StoredRow>, HostError> {
        let key = key.to_owned();
        self.call("could not read the session", move |conn| {
            load_rows(conn, &key)
        })
        .await
    }
}

impl Drop for SessionStore {
    fn drop(&mut self) {
        self.close();
    }
}

/// The database and its sidecars, as they sit in `dir`.
#[must_use]
pub fn session_files(dir: &Path) -> Vec<PathBuf> {
    ["", "-wal", "-shm"]
        .iter()
        .map(|suffix| dir.join(format!("{FILE_NAME}{suffix}")))
        .collect()
}

/// One stored event with what its row records.
#[derive(Clone, Debug)]
#[cfg_attr(not(test), allow(dead_code))]
pub struct StoredRow {
    #[allow(dead_code)] // the row order; tests read the rows in it
    pub seq: i64,
    pub turn_execution_id: String,
    pub executed_by: String,
    pub response_message_id: Option<String>,
    pub invocation_id: String,
    pub event: Event,
}

fn load_rows(conn: &Connection, key: &str) -> rusqlite::Result<Vec<StoredRow>> {
    let mut statement = conn.prepare(
        "SELECT seq, turn_execution_id, executed_by, response_message_id,
                COALESCE(invocation_id, ''), event
         FROM events WHERE session_key = ?1 ORDER BY seq",
    )?;
    let rows = statement
        .query_map(params![key], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, String>(2)?,
                row.get::<_, Option<String>>(3)?,
                row.get::<_, String>(4)?,
                row.get::<_, String>(5)?,
            ))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    Ok(rows
        .into_iter()
        .filter_map(
            |(seq, turn_execution_id, executed_by, response_message_id, invocation_id, text)| {
                // A row that no longer parses is skipped rather than failing
                // the whole session (the server re-seeds what is missing).
                match serde_json::from_str::<Event>(&text) {
                    Ok(event) => Some(StoredRow {
                        seq,
                        turn_execution_id,
                        executed_by,
                        response_message_id,
                        invocation_id,
                        event,
                    }),
                    Err(error) => {
                        log::warn!("a session event could not be read and was skipped: {error}");
                        None
                    }
                }
            },
        )
        .collect())
}

/// A tool response cut at [`MAX_TOOL_RESPONSE_BYTES`], binary content
/// replaced by a note: what the store keeps of an event.
#[must_use]
pub fn bounded(mut event: Event) -> Event {
    if let Some(content) = event.llm_response.content.as_mut() {
        for part in &mut content.parts {
            match part {
                Part::FunctionResponse {
                    function_response, ..
                } => {
                    function_response.inline_data.clear();
                    function_response.file_data.clear();
                    let text = function_response.response.to_string();
                    if text.len() > MAX_TOOL_RESPONSE_BYTES {
                        let mut end = MAX_TOOL_RESPONSE_BYTES;
                        while !text.is_char_boundary(end) {
                            end -= 1;
                        }
                        function_response.response = Value::String(format!(
                            "{}…[truncated {} bytes; re-run or re-read]",
                            &text[..end],
                            text.len() - end
                        ));
                    }
                }
                Part::InlineData { .. } => {
                    *part = Part::Text {
                        text: BINARY_NOTE.to_owned(),
                    };
                }
                _ => {}
            }
        }
    }
    event
}

fn insert_event(tx: &Transaction<'_>, key: &str, seq: i64, new: NewEvent) -> rusqlite::Result<()> {
    let event = bounded(new.event);
    let json = serde_json::to_string(&event)
        .map_err(|e| rusqlite::Error::ToSqlConversionFailure(Box::new(e)))?;
    tx.execute(
        "INSERT INTO events (session_key, seq, turn_execution_id, executed_by,
             response_message_id, invocation_id, author, event, bytes)
         VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
        params![
            key,
            seq,
            new.turn_execution_id,
            new.executed_by,
            new.response_message_id,
            event.invocation_id,
            event.author,
            json,
            i64::try_from(json.len()).unwrap_or(i64::MAX),
        ],
    )?;
    Ok(())
}

fn refresh_bytes(tx: &Transaction<'_>, key: &str) -> rusqlite::Result<()> {
    tx.execute(
        "UPDATE sessions SET bytes =
             (SELECT COALESCE(SUM(bytes), 0) FROM events WHERE session_key = ?1)
         WHERE session_key = ?1",
        params![key],
    )?;
    Ok(())
}

/// Keep the session within [`MAX_SESSION_BYTES`]: the oldest whole
/// invocations go first (an invocation is a turn's events, so a call and its
/// response never part), and the newest always stays. Whether anything went.
/// The session state the dropped events built survives them
/// ([`carry_state`]).
fn trim(tx: &Transaction<'_>, key: &str) -> rusqlite::Result<bool> {
    trim_to(tx, key, MAX_SESSION_BYTES)
}

/// The `turn_execution_id` (and answer id) of the event that carries the
/// session state of events that went (`carry_state`).
pub const CARRIED_STATE_TURN: &str = "carried-state";

/// The session state some dropped events built, as it stood after the last
/// of them, and the author of the last one that set any of it.
#[derive(Default)]
struct Carried {
    state: HashMap<String, Value>,
    author: String,
}

impl Carried {
    /// `rows` (event JSON, in order) folded in.
    fn fold(&mut self, rows: &[String]) {
        for text in rows {
            let Ok(event) = serde_json::from_str::<Event>(text) else {
                continue;
            };
            let (_, _, session) = state_utils::extract_state_deltas(&event.actions.state_delta);
            if !session.is_empty() {
                self.state.extend(session);
                self.author = event.author;
            }
        }
    }
}

/// Insert, at `seq`, one event with no content whose delta is the state
/// `carried` holds (a key set to `null` is a cleared one: left out), so the
/// session's state stays what it was when the events that built it go.
/// Like the model choice's own event (`d0/session.rs` `keep_model_choice`)
/// no model request ever folds it; it is an invocation of its own, so a
/// later trim folds it again with whatever goes then.
fn carry_state(
    tx: &Transaction<'_>,
    key: &str,
    seq: i64,
    carried: Carried,
) -> rusqlite::Result<()> {
    let state: HashMap<String, Value> = carried
        .state
        .into_iter()
        .filter(|(_, value)| !value.is_null())
        .collect();
    if state.is_empty() {
        return Ok(());
    }
    let mut event = Event::new(format!("{CARRIED_STATE_TURN}-{}", uuid::Uuid::new_v4()));
    event.author = carried.author;
    event.actions.state_delta = state;
    insert_event(
        tx,
        key,
        seq,
        NewEvent {
            turn_execution_id: CARRIED_STATE_TURN.to_owned(),
            executed_by: EXECUTED_BY_DESKTOP,
            response_message_id: Some(CARRIED_STATE_TURN.to_owned()),
            event,
        },
    )
}

/// The event JSON of one trim group (`inv`, as [`trim_to`] names it), in
/// order.
fn group_rows(tx: &Transaction<'_>, key: &str, inv: &str) -> rusqlite::Result<Vec<String>> {
    tx.prepare(
        "SELECT event FROM events WHERE session_key = ?1
         AND COALESCE(NULLIF(invocation_id, ''), 'seq:' || seq) = ?2 ORDER BY seq",
    )?
    .query_map(params![key, inv], |row| row.get(0))?
    .collect()
}

/// [`trim`] against `limit` (tests use a small one).
pub(crate) fn trim_to(tx: &Transaction<'_>, key: &str, limit: i64) -> rusqlite::Result<bool> {
    let mut statement = tx.prepare(
        "SELECT COALESCE(NULLIF(invocation_id, ''), 'seq:' || seq) AS inv,
                SUM(bytes), MIN(seq)
         FROM events WHERE session_key = ?1
         GROUP BY inv ORDER BY MIN(seq)",
    )?;
    let groups = statement
        .query_map(params![key], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, i64>(1)?,
                row.get::<_, i64>(2)?,
            ))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    drop(statement);
    let mut total: i64 = groups.iter().map(|(_, bytes, _)| bytes).sum();
    // The first group holds the session's lowest seq: the carried state
    // takes it, ahead of every event that stays.
    let head = groups.first().map(|(_, _, seq)| *seq);
    let mut carried = Carried::default();
    let mut trimmed = false;
    let newest = groups.len().saturating_sub(1);
    for (index, (inv, bytes, _)) in groups.into_iter().enumerate() {
        if total <= limit || index >= newest {
            break;
        }
        carried.fold(&group_rows(tx, key, &inv)?);
        if let Some(seq) = inv.strip_prefix("seq:") {
            tx.execute(
                "DELETE FROM events WHERE session_key = ?1 AND seq = ?2
                 AND COALESCE(invocation_id, '') = ''",
                params![key, seq.parse::<i64>().unwrap_or(-1)],
            )?;
        } else {
            tx.execute(
                "DELETE FROM events WHERE session_key = ?1 AND invocation_id = ?2",
                params![key, inv],
            )?;
        }
        total -= bytes;
        trimmed = true;
    }
    if let (true, Some(seq)) = (trimmed, head) {
        carry_state(tx, key, seq, carried)?;
    }
    Ok(trimmed)
}

/// Replace every tool response's body in the session with [`WITHHELD_BODY`]
/// (messages and calls stay).
fn withhold_bodies(tx: &Transaction<'_>, key: &str) -> rusqlite::Result<()> {
    let mut statement = tx.prepare("SELECT seq, event FROM events WHERE session_key = ?1")?;
    let rows = statement
        .query_map(params![key], |row| {
            Ok((row.get::<_, i64>(0)?, row.get::<_, String>(1)?))
        })?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    drop(statement);
    for (seq, text) in rows {
        let Ok(mut event) = serde_json::from_str::<Event>(&text) else {
            continue;
        };
        let mut changed = false;
        if let Some(content) = event.llm_response.content.as_mut() {
            for part in &mut content.parts {
                if let Part::FunctionResponse {
                    function_response, ..
                } = part
                {
                    function_response.response = Value::String(WITHHELD_BODY.to_owned());
                    function_response.inline_data.clear();
                    function_response.file_data.clear();
                    changed = true;
                }
            }
        }
        if changed {
            let json = serde_json::to_string(&event)
                .map_err(|e| rusqlite::Error::ToSqlConversionFailure(Box::new(e)))?;
            tx.execute(
                "UPDATE events SET event = ?3, bytes = ?4 WHERE session_key = ?1 AND seq = ?2",
                params![
                    key,
                    seq,
                    json,
                    i64::try_from(json.len()).unwrap_or(i64::MAX)
                ],
            )?;
        }
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// The adk session service over the store
// ---------------------------------------------------------------------------

/// `adk_session::SessionService` over [`SessionStore`]: what the runner
/// reopens and appends to. `session_id` is the [`session_key`].
pub struct SqliteSessionService {
    store: Arc<SessionStore>,
}

fn adk_error(error: &HostError) -> adk_core::AdkError {
    adk_core::AdkError::session(error.to_string())
}

fn not_found(session_id: &str) -> adk_core::AdkError {
    adk_core::AdkError::not_found(
        adk_core::ErrorComponent::Session,
        "session.not_found",
        format!("session '{session_id}' was not found"),
    )
}

/// A session as the runner reads it.
struct StoredSession {
    id: String,
    app_name: String,
    user_id: String,
    state: HashMap<String, Value>,
    events: Vec<Event>,
    updated_at: DateTime<Utc>,
}

impl Session for StoredSession {
    fn id(&self) -> &str {
        &self.id
    }

    fn app_name(&self) -> &str {
        &self.app_name
    }

    fn user_id(&self) -> &str {
        &self.user_id
    }

    fn state(&self) -> &dyn State {
        self
    }

    fn events(&self) -> &dyn Events {
        self
    }

    fn last_update_time(&self) -> DateTime<Utc> {
        self.updated_at
    }
}

impl State for StoredSession {
    fn get(&self, key: &str) -> Option<Value> {
        self.state.get(key).cloned()
    }

    fn set(&mut self, key: String, value: Value) {
        if adk_core::validate_state_key(&key).is_ok() {
            self.state.insert(key, value);
        }
    }

    fn all(&self) -> HashMap<String, Value> {
        self.state.clone()
    }
}

impl Events for StoredSession {
    fn all(&self) -> Vec<Event> {
        self.events.clone()
    }

    fn len(&self) -> usize {
        self.events.len()
    }

    fn at(&self, index: usize) -> Option<&Event> {
        self.events.get(index)
    }
}

/// The session-tier state its events' deltas built (not stored apart: the
/// events are the truth, as `rewind` rebuilds it).
fn state_of(events: &[Event]) -> HashMap<String, Value> {
    let mut state = HashMap::new();
    for event in events {
        let (_, _, session) = state_utils::extract_state_deltas(&event.actions.state_delta);
        state.extend(session);
    }
    state
}

impl SqliteSessionService {
    async fn load(&self, key: &str) -> Result<Option<StoredSession>, adk_core::AdkError> {
        let key = key.to_owned();
        let loaded = self
            .store
            .call("could not read the session", move |conn| {
                let updated: Option<i64> = conn
                    .query_row(
                        "SELECT updated_at FROM sessions WHERE session_key = ?1",
                        params![key],
                        |row| row.get(0),
                    )
                    .optional()?;
                match updated {
                    None => Ok(None),
                    Some(updated) => Ok(Some((key.clone(), updated, load_rows(conn, &key)?))),
                }
            })
            .await
            .map_err(|e| adk_error(&e))?;
        Ok(loaded.map(|(key, updated, rows)| {
            let events: Vec<Event> = rows.into_iter().map(|row| row.event).collect();
            StoredSession {
                id: key,
                app_name: APP_NAME.to_owned(),
                user_id: ADK_USER.to_owned(),
                state: state_of(&events),
                events,
                updated_at: Utc
                    .timestamp_millis_opt(updated)
                    .single()
                    .unwrap_or_else(Utc::now),
            }
        }))
    }
}

#[async_trait]
impl SessionService for SqliteSessionService {
    /// An existing session is returned as it is (the host opens sessions
    /// with [`SessionStore::open_session`], which records whose they are).
    async fn create(&self, req: CreateRequest) -> adk_core::Result<Box<dyn Session>> {
        let id = req
            .session_id
            .unwrap_or_else(|| uuid::Uuid::new_v4().to_string());
        let key = id.clone();
        self.store
            .call("could not create the session", move |conn| {
                conn.execute(
                    "INSERT OR IGNORE INTO sessions (session_key, deployment, user_id,
                         project_id, conversation_uuid, thread_nonce, created_at, updated_at)
                     VALUES (?1, '', 0, 0, '', zeroblob(16), ?2, ?2)",
                    params![key, now_ms()],
                )
            })
            .await
            .map_err(|e| adk_error(&e))?;
        let session = self.load(&id).await?.ok_or_else(|| not_found(&id))?;
        Ok(Box::new(session))
    }

    async fn get(&self, req: GetRequest) -> adk_core::Result<Box<dyn Session>> {
        let mut session = self
            .load(&req.session_id)
            .await?
            .ok_or_else(|| not_found(&req.session_id))?;
        if let Some(recent) = req.num_recent_events {
            let start = session.events.len().saturating_sub(recent);
            session.events.drain(..start);
        }
        if let Some(after) = req.after {
            session.events.retain(|event| event.timestamp >= after);
        }
        Ok(Box::new(session))
    }

    async fn list(&self, req: ListRequest) -> adk_core::Result<Vec<Box<dyn Session>>> {
        let limit = i64::try_from(req.limit.unwrap_or(usize::MAX)).unwrap_or(i64::MAX);
        let offset = i64::try_from(req.offset.unwrap_or(0)).unwrap_or(i64::MAX);
        let keys: Vec<String> = self
            .store
            .call("could not list the sessions", move |conn| {
                let mut statement = conn.prepare(
                    "SELECT session_key FROM sessions ORDER BY updated_at DESC LIMIT ?1 OFFSET ?2",
                )?;
                statement
                    .query_map(params![limit, offset], |row| row.get(0))?
                    .collect()
            })
            .await
            .map_err(|e| adk_error(&e))?;
        let mut out: Vec<Box<dyn Session>> = Vec::new();
        for key in keys {
            if let Some(session) = self.load(&key).await? {
                out.push(Box::new(session));
            }
        }
        Ok(out)
    }

    async fn delete(&self, req: DeleteRequest) -> adk_core::Result<()> {
        let key = req.session_id;
        self.store
            .call("could not delete the session", move |conn| {
                conn.execute("DELETE FROM sessions WHERE session_key = ?1", params![key])
                    .map(drop)
            })
            .await
            .map_err(|e| adk_error(&e))
    }

    async fn delete_all_sessions(&self, _app_name: &str, _user_id: &str) -> adk_core::Result<()> {
        self.store
            .call("could not delete the sessions", |conn| {
                conn.execute("DELETE FROM sessions", []).map(drop)
            })
            .await
            .map_err(|e| adk_error(&e))
    }

    /// Append `event` to the session, tagged with the running turn that
    /// holds it (the runner appends the turn's user message and each event
    /// it yields). A session without a running turn is refused: nothing but
    /// a turn writes a desktop session.
    async fn append_event(&self, session_id: &str, mut event: Event) -> adk_core::Result<()> {
        event
            .actions
            .state_delta
            .retain(|key, _| !key.starts_with(adk_session::KEY_PREFIX_TEMP));
        let key = session_id.to_owned();
        let execution = self.store.active.lock().get(&key).cloned();
        let Some(execution_id) = execution else {
            return Err(adk_core::AdkError::session(
                "this session has no running turn to append to",
            ));
        };
        self.store
            .call("could not append to the session", move |conn| {
                let tx = conn.transaction()?;
                let seq: i64 = tx.query_row(
                    "SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_key = ?1",
                    params![key],
                    |row| row.get(0),
                )?;
                insert_event(
                    &tx,
                    &key,
                    seq,
                    NewEvent {
                        turn_execution_id: execution_id,
                        executed_by: EXECUTED_BY_DESKTOP,
                        response_message_id: None,
                        event,
                    },
                )?;
                refresh_bytes(&tx, &key)?;
                tx.execute(
                    "UPDATE sessions SET updated_at = ?2 WHERE session_key = ?1",
                    params![key, now_ms()],
                )?;
                tx.commit()
            })
            .await
            .map_err(|e| adk_error(&e))
    }

    async fn append_event_for_identity(&self, req: AppendEventRequest) -> adk_core::Result<()> {
        self.append_event(req.identity.session_id.as_ref(), req.event)
            .await
    }
}

/// The desktop's [`StateStore`]: SQLite sessions, and an in-memory
/// checkpointer until pipelines run locally (#1239).
pub struct DesktopStateStore {
    store: Arc<SessionStore>,
    service: Arc<SqliteSessionService>,
    checkpointer: Arc<MemoryCheckpointer>,
}

impl DesktopStateStore {
    /// Open `sessions.sqlite` in `dir`.
    ///
    /// # Errors
    ///
    /// As [`SessionStore::open`].
    pub fn open(dir: &Path) -> Result<Self, HostError> {
        Ok(Self::new(Arc::new(SessionStore::open(dir)?)))
    }

    #[must_use]
    pub fn new(store: Arc<SessionStore>) -> Self {
        Self {
            service: Arc::new(SqliteSessionService {
                store: store.clone(),
            }),
            store,
            checkpointer: Arc::new(MemoryCheckpointer::new()),
        }
    }

    /// The store behind the service: opening, seeding and settling.
    #[must_use]
    pub fn store(&self) -> &Arc<SessionStore> {
        &self.store
    }
}

impl StateStore for DesktopStateStore {
    fn sessions(&self) -> Arc<dyn SessionService> {
        self.service.clone()
    }

    fn checkpointer(&self) -> Arc<dyn Checkpointer> {
        self.checkpointer.clone()
    }
}

#[cfg(test)]
mod tests;
