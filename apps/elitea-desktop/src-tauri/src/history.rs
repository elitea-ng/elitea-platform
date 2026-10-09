//! The local history of Local work threads: `threads.sqlite` in the app data
//! directory (next to `workspaces.json` and the credentials file).
//!
//! A thread is a conversation a workspace session ran turns in. The server
//! keeps its messages; this store keeps what only the desktop saw: each
//! turn's `agent://event` stream (tool rows, approvals, statuses), the prompt
//! and its "@" mentions as typed, and the files the turn changed. Replaying a
//! turn is feeding its stored events through the webview's own turn reducer.
//!
//! Events are recorded HOST-side as they are emitted ([`TurnTap`]), so the
//! history survives a webview reload or crash. The emit path never touches
//! SQLite: [`TurnTap::emit`] only queues the event on a bounded channel (if
//! it is full, the tap keeps it and merges text until there is room), and a
//! dedicated writer thread owns its own connection. It batches what arrives
//! into one transaction per flush: on every non-text event (so `done` and
//! `error` too), every [`FLUSH_INTERVAL`] or [`FLUSH_BYTES`], on a read or a
//! delete, at a turn's end and at the app's exit. Text is stored as APPENDED
//! chunks: the deltas of one batch are one row, numbered by the last delta
//! it holds; an earlier row is never rewritten. Replay folds the chunks the
//! way the reducer folds live deltas. A crash loses at most the last batch.
//!
//! Every row is keyed by the deployment origin, the signed-in user id, the
//! workspace id and the conversation id: another account or another
//! deployment never reads another's threads. Signing out keeps the rows (the
//! next sign-in of the same account sees them again); removing a workspace
//! deletes its rows for every account.
//!
//! Bounds: one event's strings are cut at [`MAX_EVENT_STRING`] bytes, a
//! turn's events at [`MAX_TURN_EVENT_BYTES`] (past it only `status`, `error`
//! and `done` are kept, and the turn is marked `events_truncated`), a turn's
//! stored diffs at [`MAX_DIFF_BYTES`] each and [`MAX_CHANGES_BYTES`] in all,
//! and a thread keeps its newest [`MAX_TURNS_PER_THREAD`] turns within
//! [`MAX_THREAD_BYTES`].
//!
//! Protection, on Unix: the directory is `0700`, the database is created
//! `0600` before SQLite opens it (SQLite gives its `-wal`/`-shm` files the
//! database's mode), it is opened without following a symlink, and a file
//! that is a symlink, not a regular file, another user's, or readable or
//! writable by group or others is refused (the app then runs without
//! history; a warning is logged). Contents are never logged.
//!
//! Schema (`PRAGMA user_version` = [`SCHEMA_VERSION`], WAL journal):
//!
//! ```sql
//! turns  (turn_id TEXT PRIMARY KEY, origin TEXT, user_id INTEGER,
//!         workspace_id TEXT, conversation_id TEXT, conversation_uuid TEXT NULL,
//!         prompt TEXT, mentions TEXT /* JSON string[] */,
//!         started_at INTEGER /* Unix ms */, finished_at INTEGER NULL,
//!         changes TEXT NULL /* JSON FileChange[] */,
//!         events_bytes INTEGER, events_truncated INTEGER /* 0|1 */)
//! events (turn_id TEXT REFERENCES turns ON DELETE CASCADE, seq INTEGER,
//!         kind TEXT, payload TEXT /* JSON */, PRIMARY KEY (turn_id, seq))
//! ```

use std::collections::{HashSet, VecDeque};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::mpsc::{Receiver, RecvTimeoutError, SyncSender, TrySendError};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use rusqlite::{Connection, OpenFlags, params};
use serde::Serialize;
use serde_json::Value;

use crate::credentials_file::{create_private_dir, create_private_file};
use crate::d0::events::{AgentEvent, EventEmitter};
use crate::d0::recorder::FileChange;
use crate::error::HostError;

pub const FILE_NAME: &str = "threads.sqlite";
/// `PRAGMA user_version` of the schema this build writes.
pub const SCHEMA_VERSION: i64 = 1;
/// A string inside one event's payload is cut to this many bytes.
pub const MAX_EVENT_STRING: usize = 16 * 1024;
/// One turn's stored events, in payload bytes.
pub const MAX_TURN_EVENT_BYTES: usize = 4 * 1024 * 1024;
/// One changed file's stored diff.
pub const MAX_DIFF_BYTES: usize = 64 * 1024;
/// All of one turn's stored diffs; later files keep their counts, not their diff.
pub const MAX_CHANGES_BYTES: usize = 1024 * 1024;
/// The newest turns a thread keeps.
pub const MAX_TURNS_PER_THREAD: usize = 200;
/// What a thread's turns may take in all (events and changes); the oldest
/// turns go first.
pub const MAX_THREAD_BYTES: i64 = 32 * 1024 * 1024;
/// The writer commits what is pending at least this often...
pub const FLUSH_INTERVAL: Duration = Duration::from_millis(500);
/// ...or once this many bytes of events are pending.
pub const FLUSH_BYTES: usize = 64 * 1024;
/// Ops the writer's queue holds before a tap keeps them itself.
const CHANNEL_CAPACITY: usize = 1024;

const SCHEMA: &str = "
CREATE TABLE turns (
    turn_id TEXT PRIMARY KEY NOT NULL,
    origin TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    workspace_id TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    conversation_uuid TEXT,
    prompt TEXT NOT NULL,
    mentions TEXT NOT NULL,
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    changes TEXT,
    events_bytes INTEGER NOT NULL DEFAULT 0,
    events_truncated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX turns_by_thread
    ON turns (origin, user_id, workspace_id, conversation_id, started_at);
CREATE INDEX turns_by_workspace ON turns (workspace_id);
CREATE TABLE events (
    turn_id TEXT NOT NULL REFERENCES turns (turn_id) ON DELETE CASCADE,
    seq INTEGER NOT NULL,
    kind TEXT NOT NULL,
    payload TEXT NOT NULL,
    PRIMARY KEY (turn_id, seq)
) WITHOUT ROWID;
";

/// Whose history a row is: the deployment and the signed-in user there.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Owner {
    /// The deployment origin, no trailing slash.
    pub origin: String,
    pub user_id: i64,
}

/// A turn's row, written once its start succeeded.
#[derive(Clone, Debug)]
pub struct NewTurn {
    pub owner: Owner,
    pub workspace_id: String,
    pub conversation_id: String,
    pub conversation_uuid: Option<String>,
    pub turn_id: String,
    /// As the person typed it (the "@" list is `mentions`, not inlined).
    pub prompt: String,
    pub mentions: Vec<String>,
    pub started_at: i64,
}

/// One stored event, in the `agent://event` shape.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct StoredEvent {
    pub turn_id: String,
    pub seq: u64,
    pub kind: String,
    pub payload: Value,
}

/// One stored turn of a thread, as `thread_history` returns it.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct StoredTurn {
    pub turn_id: String,
    pub conversation_id: String,
    pub conversation_uuid: Option<String>,
    pub prompt: String,
    pub mentions: Vec<String>,
    pub started_at: i64,
    pub finished_at: Option<i64>,
    pub events: Vec<StoredEvent>,
    /// The files the turn changed, as they were when it ended (`null`
    /// before it ended).
    pub changes: Option<Value>,
    pub events_truncated: bool,
    /// `done` (its `done` event is stored), `running` (this host is still
    /// running it) or `interrupted` (it never ended: the app quit or
    /// crashed). Set by the agent host, which knows what runs.
    pub state: &'static str,
    /// The host still keeps the turn: `turn_changes` and
    /// `checkpoint_restore` answer for it.
    pub live: bool,
}

fn storage(what: &str, error: &rusqlite::Error) -> HostError {
    HostError::Storage(format!("{what}: {error}"))
}

fn now_ms() -> i64 {
    chrono::Utc::now().timestamp_millis()
}

/// Cut `text` to at most `max` bytes on a character boundary, marking the cut.
fn cut(text: &str, max: usize) -> Option<String> {
    if text.len() <= max {
        return None;
    }
    let mut end = max;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    Some(format!("{}…", &text[..end]))
}

/// Every string in `value` cut to [`MAX_EVENT_STRING`] bytes.
fn bounded(mut value: Value) -> Value {
    fn walk(value: &mut Value) {
        match value {
            Value::String(text) => {
                if let Some(shorter) = cut(text, MAX_EVENT_STRING) {
                    *text = shorter;
                }
            }
            Value::Array(items) => items.iter_mut().for_each(walk),
            Value::Object(map) => map.values_mut().for_each(walk),
            _ => {}
        }
    }
    walk(&mut value);
    value
}

/// A turn's changed files as stored: diffs capped one by one and in all.
#[must_use]
pub fn stored_changes(changes: &[FileChange]) -> Value {
    let mut budget = MAX_CHANGES_BYTES;
    let files: Vec<Value> = changes
        .iter()
        .map(|change| {
            let mut diff = cut(&change.diff, MAX_DIFF_BYTES).unwrap_or_else(|| change.diff.clone());
            if diff.len() > budget {
                diff = String::new();
            }
            budget -= diff.len();
            serde_json::json!({
                "path": change.path,
                "status": change.status,
                "added": change.added,
                "removed": change.removed,
                "diff": diff,
            })
        })
        .collect();
    Value::Array(files)
}

/// The refusals of the credentials file, for the database: an existing
/// file must be ours, regular and owner-only.
fn inspect(path: &Path) -> Result<(), HostError> {
    let refuse = |why: &str| {
        log::warn!("refusing the thread history {}: {why}", path.display());
        HostError::Storage(format!("{} {why}", path.display()))
    };
    let meta = match fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(HostError::Storage(e.to_string())),
    };
    if meta.file_type().is_symlink() {
        return Err(refuse("is a symbolic link"));
    }
    if !meta.is_file() {
        return Err(refuse("is not a regular file"));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt as _;
        if meta.uid() != rustix::process::geteuid().as_raw() {
            return Err(refuse("belongs to another user"));
        }
        if meta.mode() & 0o077 != 0 {
            return Err(refuse("is readable or writable by other users"));
        }
    }
    Ok(())
}
/// One write for the history's writer thread.
enum Op {
    Begin(NewTurn),
    Event {
        turn_id: String,
        seq: u64,
        kind: &'static str,
        payload: String,
    },
    /// Text deltas, merged: one appended row, numbered by the last delta.
    Text {
        turn_id: String,
        seq: u64,
        text: String,
    },
    Truncated {
        turn_id: String,
    },
    Changes {
        turn_id: String,
        json: String,
    },
    /// The turn's `done` was recorded: mark it finished, trim its thread.
    Finish {
        turn_id: String,
        owner: Owner,
        workspace_id: String,
        conversation_id: String,
    },
    /// Commit what is pending, then answer.
    Flush(std::sync::mpsc::Sender<()>),
}

impl Op {
    fn turn_id(&self) -> Option<&str> {
        match self {
            Self::Begin(turn) => Some(&turn.turn_id),
            Self::Event { turn_id, .. }
            | Self::Text { turn_id, .. }
            | Self::Truncated { turn_id }
            | Self::Changes { turn_id, .. }
            | Self::Finish { turn_id, .. } => Some(turn_id),
            Self::Flush(_) => None,
        }
    }

    /// `next` merged into this one when both are text of the same turn.
    fn absorb(&mut self, next: Op) -> Option<Op> {
        match (self, next) {
            (
                Self::Text { turn_id, seq, text },
                Self::Text {
                    turn_id: next_turn,
                    seq: next_seq,
                    text: more,
                },
            ) if *turn_id == next_turn => {
                text.push_str(&more);
                *seq = next_seq;
                None
            }
            (_, next) => Some(next),
        }
    }
}

fn open_connection(path: &Path) -> Result<Connection, HostError> {
    let conn =
        Connection::open_with_flags(path, OpenFlags::default() | OpenFlags::SQLITE_OPEN_NOFOLLOW)
            .map_err(|e| storage("could not open the thread history", &e))?;
    conn.busy_timeout(Duration::from_secs(5))
        .map_err(|e| storage("could not configure the thread history", &e))?;
    conn.pragma_update(None, "journal_mode", "WAL")
        .map_err(|e| storage("could not configure the thread history", &e))?;
    conn.pragma_update(None, "synchronous", "NORMAL")
        .map_err(|e| storage("could not configure the thread history", &e))?;
    conn.pragma_update(None, "foreign_keys", "ON")
        .map_err(|e| storage("could not configure the thread history", &e))?;
    Ok(conn)
}

/// What [`HistoryStore::try_send`] did with an op.
enum Sent {
    Queued,
    /// No room: the op, back.
    Full(Op),
    /// The writer is gone (the store is closing).
    Closed,
}

pub struct HistoryStore {
    path: PathBuf,
    /// Reads and deletes (the writer thread has its own connection).
    conn: Mutex<Connection>,
    /// The writer thread's queue; `None` once the store is dropped.
    sender: Option<SyncSender<Op>>,
    writer: Option<std::thread::JoinHandle<()>>,
}

impl HistoryStore {
    /// Open (or create) `threads.sqlite` in `dir`, and start its writer
    /// thread.
    ///
    /// # Errors
    ///
    /// The directory or file is refused (see the module docs), the file is
    /// not a database, or it was written by a newer schema.
    pub fn open(dir: &Path) -> Result<Self, HostError> {
        create_private_dir(dir)?;
        // Canonical, so the no-follow open below refuses a symlinked FILE,
        // not a symlink higher up (macOS's `/var` is one).
        let dir = &fs::canonicalize(dir).map_err(|e| HostError::Storage(e.to_string()))?;
        let path = dir.join(FILE_NAME);
        inspect(&path)?;
        // Created owner-only BEFORE SQLite opens it, so it is never wider.
        match create_private_file(&path) {
            Ok(_) => {}
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
            Err(e) => return Err(HostError::Storage(e.to_string())),
        }
        for sidecar in ["-wal", "-shm"] {
            inspect(&dir.join(format!("{FILE_NAME}{sidecar}")))?;
        }
        let conn = open_connection(&path)?;
        migrate(&conn)?;
        let writer_conn = open_connection(&path)?;
        let (sender, receiver) = std::sync::mpsc::sync_channel(CHANNEL_CAPACITY);
        let writer = std::thread::Builder::new()
            .name("history-writer".into())
            .spawn(move || Writer::new(writer_conn).run(&receiver))
            .map_err(|e| HostError::Storage(format!("could not start the history writer: {e}")))?;
        Ok(Self {
            path,
            conn: Mutex::new(conn),
            sender: Some(sender),
            writer: Some(writer),
        })
    }

    #[must_use]
    pub fn path(&self) -> &Path {
        &self.path
    }

    fn with<T>(
        &self,
        what: &str,
        work: impl FnOnce(&mut Connection) -> rusqlite::Result<T>,
    ) -> Result<T, HostError> {
        let mut conn = self
            .conn
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        work(&mut conn).map_err(|e| storage(what, &e))
    }

    /// Queue `op` without waiting; handed back when the queue is full.
    fn try_send(&self, op: Op) -> Sent {
        let Some(sender) = &self.sender else {
            return Sent::Closed;
        };
        match sender.try_send(op) {
            Ok(()) => Sent::Queued,
            Err(TrySendError::Full(op)) => Sent::Full(op),
            Err(TrySendError::Disconnected(_)) => Sent::Closed,
        }
    }

    /// Queue `op`, waiting for room (turn end and flushes only, never an emit).
    fn send(&self, op: Op) -> bool {
        self.sender
            .as_ref()
            .is_some_and(|sender| sender.send(op).is_ok())
    }

    /// Commit everything queued so far and wait for it (a read, a delete,
    /// a turn's end, the app's exit).
    pub fn flush(&self) {
        let (ack, done) = std::sync::mpsc::channel();
        if self.send(Op::Flush(ack)) && done.recv_timeout(Duration::from_secs(5)).is_err() {
            log::warn!("the thread history writer did not answer a flush in time");
        }
    }

    /// The turns of one thread of `owner`, oldest first, with their events.
    /// `conversation` matches the id the turns were started with or the
    /// conversation's UUID. `state` and `live` are left for the caller
    /// (`interrupted` / `false` unless a `done` was stored).
    ///
    /// # Errors
    ///
    /// The database could not be read.
    pub fn thread(
        &self,
        owner: &Owner,
        workspace_id: &str,
        conversation: &str,
    ) -> Result<Vec<StoredTurn>, HostError> {
        // A running turn's events so far, not just what the last batch wrote.
        self.flush();
        self.with("could not read the thread history", |conn| {
            let mut turns: Vec<StoredTurn> = {
                let mut statement = conn.prepare(
                    "SELECT turn_id, conversation_id, conversation_uuid, prompt, mentions,
                            started_at, finished_at, changes, events_truncated
                     FROM turns
                     WHERE origin = ?1 AND user_id = ?2 AND workspace_id = ?3
                       AND (conversation_id = ?4 OR conversation_uuid = ?4)
                     ORDER BY started_at, rowid",
                )?;
                statement
                    .query_map(
                        params![owner.origin, owner.user_id, workspace_id, conversation],
                        |row| {
                            let mentions: String = row.get(4)?;
                            let changes: Option<String> = row.get(7)?;
                            Ok(StoredTurn {
                                turn_id: row.get(0)?,
                                conversation_id: row.get(1)?,
                                conversation_uuid: row.get(2)?,
                                prompt: row.get(3)?,
                                mentions: serde_json::from_str(&mentions).unwrap_or_default(),
                                started_at: row.get(5)?,
                                finished_at: row.get(6)?,
                                events: Vec::new(),
                                changes: changes.and_then(|c| serde_json::from_str(&c).ok()),
                                events_truncated: row.get::<_, i64>(8)? != 0,
                                state: "interrupted",
                                live: false,
                            })
                        },
                    )?
                    .collect::<rusqlite::Result<_>>()?
            };
            let mut statement = conn
                .prepare("SELECT seq, kind, payload FROM events WHERE turn_id = ?1 ORDER BY seq")?;
            for turn in &mut turns {
                turn.events = statement
                    .query_map(params![turn.turn_id], |row| {
                        let seq: i64 = row.get(0)?;
                        let payload: String = row.get(2)?;
                        Ok(StoredEvent {
                            turn_id: turn.turn_id.clone(),
                            seq: u64::try_from(seq).unwrap_or_default(),
                            kind: row.get(1)?,
                            payload: serde_json::from_str(&payload).unwrap_or(Value::Null),
                        })
                    })?
                    .collect::<rusqlite::Result<_>>()?;
                if turn.events.iter().any(|event| event.kind == "done") {
                    turn.state = "done";
                }
            }
            Ok(turns)
        })
    }

    /// Forget one thread of `owner`; the number of turns deleted.
    ///
    /// # Errors
    ///
    /// The database could not be written.
    pub fn delete_thread(
        &self,
        owner: &Owner,
        workspace_id: &str,
        conversation: &str,
    ) -> Result<usize, HostError> {
        self.flush();
        self.with("could not delete the thread history", |conn| {
            conn.execute(
                "DELETE FROM turns
                 WHERE origin = ?1 AND user_id = ?2 AND workspace_id = ?3
                   AND (conversation_id = ?4 OR conversation_uuid = ?4)",
                params![owner.origin, owner.user_id, workspace_id, conversation],
            )
        })
    }

    /// Forget every thread of a removed workspace, whoever's.
    ///
    /// # Errors
    ///
    /// The database could not be written.
    pub fn delete_workspace(&self, workspace_id: &str) -> Result<usize, HostError> {
        self.flush();
        self.with("could not delete the workspace's history", |conn| {
            conn.execute(
                "DELETE FROM turns WHERE workspace_id = ?1",
                params![workspace_id],
            )
        })
    }
}

impl Drop for HistoryStore {
    /// The writer commits what is left and stops once its queue closes.
    fn drop(&mut self) {
        drop(self.sender.take());
        if let Some(writer) = self.writer.take()
            && writer.join().is_err()
        {
            log::warn!("the thread history writer panicked");
        }
    }
}

/// The writer thread: owns its connection, batches ops into one
/// transaction per flush (see the module docs).
struct Writer {
    conn: Connection,
    pending: Vec<Op>,
    pending_bytes: usize,
    since: Option<Instant>,
    /// Turns a write failed for: their later ops are dropped.
    failed: HashSet<String>,
}

impl Writer {
    fn new(conn: Connection) -> Self {
        Self {
            conn,
            pending: Vec::new(),
            pending_bytes: 0,
            since: None,
            failed: HashSet::new(),
        }
    }

    fn run(mut self, receiver: &Receiver<Op>) {
        loop {
            let next = match self.since {
                None => receiver.recv().map_err(|_| RecvTimeoutError::Disconnected),
                Some(since) => {
                    receiver.recv_timeout(FLUSH_INTERVAL.saturating_sub(since.elapsed()))
                }
            };
            match next {
                Ok(Op::Flush(ack)) => {
                    self.commit();
                    let _ = ack.send(());
                }
                Ok(op) => self.push(op),
                Err(RecvTimeoutError::Timeout) => self.commit(),
                Err(RecvTimeoutError::Disconnected) => {
                    self.commit();
                    return;
                }
            }
        }
    }

    fn push(&mut self, op: Op) {
        let text = matches!(op, Op::Text { .. });
        self.pending_bytes += match &op {
            Op::Text { text, .. } => text.len(),
            Op::Event { payload, .. } => payload.len(),
            _ => 0,
        };
        self.since.get_or_insert_with(Instant::now);
        let op = match self.pending.last_mut() {
            Some(last) => last.absorb(op),
            None => Some(op),
        };
        if let Some(op) = op {
            self.pending.push(op);
        }
        // A non-text event ends a text run (and `done` ends the turn).
        if !text || self.pending_bytes >= FLUSH_BYTES {
            self.commit();
        }
    }

    fn commit(&mut self) {
        self.since = None;
        self.pending_bytes = 0;
        if self.pending.is_empty() {
            return;
        }
        let ops = std::mem::take(&mut self.pending);
        let tx = match self.conn.transaction() {
            Ok(tx) => tx,
            Err(error) => {
                log::warn!("the thread history could not write: {error}");
                return;
            }
        };
        for op in ops {
            let Some(turn_id) = op.turn_id().map(str::to_owned) else {
                continue;
            };
            if self.failed.contains(&turn_id) {
                continue;
            }
            if let Err(error) = apply(&tx, op) {
                log::warn!("the thread history stopped recording a turn: {error}");
                self.failed.insert(turn_id);
            }
        }
        if let Err(error) = tx.commit() {
            log::warn!("the thread history could not commit: {error}");
        }
    }
}

fn add_bytes(conn: &Connection, turn_id: &str, bytes: usize) -> rusqlite::Result<usize> {
    conn.execute(
        "UPDATE turns SET events_bytes = events_bytes + ?2 WHERE turn_id = ?1",
        params![turn_id, i64::try_from(bytes).unwrap_or(i64::MAX)],
    )
}

fn insert_event(
    conn: &Connection,
    turn_id: &str,
    seq: u64,
    kind: &str,
    payload: &str,
) -> rusqlite::Result<()> {
    conn.execute(
        "INSERT OR REPLACE INTO events (turn_id, seq, kind, payload) VALUES (?1, ?2, ?3, ?4)",
        params![
            turn_id,
            i64::try_from(seq).unwrap_or(i64::MAX),
            kind,
            payload
        ],
    )?;
    add_bytes(conn, turn_id, payload.len()).map(|_| ())
}

/// One op against the writer's transaction. Text is only ever INSERTed
/// (an appended chunk), never an UPDATE of earlier text.
fn apply(conn: &Connection, op: Op) -> rusqlite::Result<()> {
    match op {
        Op::Begin(turn) => {
            let mentions = serde_json::to_string(&turn.mentions).unwrap_or_else(|_| "[]".into());
            conn.execute(
                "INSERT INTO turns (turn_id, origin, user_id, workspace_id, conversation_id,
                     conversation_uuid, prompt, mentions, started_at)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)",
                params![
                    turn.turn_id,
                    turn.owner.origin,
                    turn.owner.user_id,
                    turn.workspace_id,
                    turn.conversation_id,
                    turn.conversation_uuid,
                    turn.prompt,
                    mentions,
                    turn.started_at,
                ],
            )
            .map(|_| ())
        }
        Op::Event {
            turn_id,
            seq,
            kind,
            payload,
        } => insert_event(conn, &turn_id, seq, kind, &payload),
        Op::Text { turn_id, seq, text } => {
            let payload = serde_json::json!({ "text": text }).to_string();
            insert_event(conn, &turn_id, seq, "text_delta", &payload)
        }
        Op::Truncated { turn_id } => conn
            .execute(
                "UPDATE turns SET events_truncated = 1 WHERE turn_id = ?1",
                params![turn_id],
            )
            .map(|_| ()),
        Op::Changes { turn_id, json } => conn
            .execute(
                "UPDATE turns SET changes = ?2 WHERE turn_id = ?1",
                params![turn_id, json],
            )
            .map(|_| ()),
        Op::Finish {
            turn_id,
            owner,
            workspace_id,
            conversation_id,
        } => {
            conn.execute(
                "UPDATE turns SET finished_at = ?2 WHERE turn_id = ?1",
                params![turn_id, now_ms()],
            )?;
            prune_thread(conn, &owner, &workspace_id, &conversation_id).map(|_| ())
        }
        Op::Flush(_) => Ok(()),
    }
}

/// Keep a thread within its bounds: the newest turns, up to
/// [`MAX_TURNS_PER_THREAD`] and [`MAX_THREAD_BYTES`].
fn prune_thread(
    conn: &Connection,
    owner: &Owner,
    workspace_id: &str,
    conversation_id: &str,
) -> rusqlite::Result<usize> {
    let sizes: Vec<(String, i64)> = {
        let mut statement = conn.prepare(
            "SELECT turn_id, events_bytes + length(coalesce(changes, ''))
             FROM turns
             WHERE origin = ?1 AND user_id = ?2 AND workspace_id = ?3 AND conversation_id = ?4
             ORDER BY started_at DESC, rowid DESC",
        )?;
        statement
            .query_map(
                params![owner.origin, owner.user_id, workspace_id, conversation_id],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )?
            .collect::<rusqlite::Result<_>>()?
    };
    let mut total = 0_i64;
    let mut dropped = 0;
    for (index, (turn_id, size)) in sizes.iter().enumerate() {
        total = total.saturating_add(*size);
        // The newest turn always stays, whatever its size.
        if index > 0 && (index >= MAX_TURNS_PER_THREAD || total > MAX_THREAD_BYTES) {
            conn.execute("DELETE FROM turns WHERE turn_id = ?1", params![turn_id])?;
            dropped += 1;
        }
    }
    Ok(dropped)
}

/// The Doctor's look at an existing database: it opens read-only (never
/// following a symlink), passes `PRAGMA integrity_check`; its schema version.
///
/// # Errors
///
/// What SQLite said, for a person.
pub fn integrity(path: &Path) -> Result<i64, String> {
    // The folder canonical, so no-follow refuses a symlinked FILE only
    // (macOS's `/var` is a symlink higher up).
    let path = match (path.parent(), path.file_name()) {
        (Some(dir), Some(name)) => fs::canonicalize(dir).map_err(|e| e.to_string())?.join(name),
        _ => path.to_owned(),
    };
    let conn = Connection::open_with_flags(
        &path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NOFOLLOW,
    )
    .map_err(|e| e.to_string())?;
    let verdict: String = conn
        .query_row("PRAGMA integrity_check", [], |row| row.get(0))
        .map_err(|e| e.to_string())?;
    if verdict != "ok" {
        return Err(verdict);
    }
    conn.query_row("PRAGMA user_version", [], |row| row.get(0))
        .map_err(|e| e.to_string())
}

fn migrate(conn: &Connection) -> Result<(), HostError> {
    let version: i64 = conn
        .query_row("PRAGMA user_version", [], |row| row.get(0))
        .map_err(|e| storage("could not read the thread history", &e))?;
    match version {
        0 => conn
            .execute_batch(&format!(
                "BEGIN; {SCHEMA} PRAGMA user_version = {SCHEMA_VERSION}; COMMIT;"
            ))
            .map_err(|e| storage("could not create the thread history", &e)),
        SCHEMA_VERSION => Ok(()),
        newer => Err(HostError::Storage(format!(
            "the thread history was written by a newer version of the app (schema {newer})"
        ))),
    }
}
/// Where a turn's recording is.
enum TapState {
    /// Before the turn started: its events wait here (a refused start is
    /// not history; it never showed as a turn).
    Buffering(Vec<AgentEvent>),
    Recording(Box<Recording>),
    /// No history (none configured, or the start was refused).
    Off,
}

struct Recording {
    turn: NewTurn,
    bytes: usize,
    capped: bool,
    /// Ops the writer's queue had no room for, oldest first (text merged).
    backlog: VecDeque<Op>,
}

/// The `agent://event` emitter of one turn: forwards every event to the
/// window (and the host's attention), and queues it for the history's
/// writer thread. Never touches SQLite itself.
pub struct TurnTap {
    inner: Arc<dyn EventEmitter>,
    store: Option<Arc<HistoryStore>>,
    state: Mutex<TapState>,
}

impl TurnTap {
    #[must_use]
    pub fn new(inner: Arc<dyn EventEmitter>, store: Option<Arc<HistoryStore>>) -> Arc<Self> {
        let state = if store.is_some() {
            TapState::Buffering(Vec::new())
        } else {
            TapState::Off
        };
        Arc::new(Self {
            inner,
            store,
            state: Mutex::new(state),
        })
    }

    fn state(&self) -> std::sync::MutexGuard<'_, TapState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The turn started: queue its row and what it sent so far, then
    /// record as it goes.
    pub fn activate(&self, turn: NewTurn) {
        let Some(store) = &self.store else { return };
        let mut state = self.state();
        let TapState::Buffering(buffered) = std::mem::replace(&mut *state, TapState::Off) else {
            return;
        };
        let mut recording = Box::new(Recording {
            turn: turn.clone(),
            bytes: 0,
            capped: false,
            backlog: VecDeque::new(),
        });
        Self::queue(store, &mut recording, Op::Begin(turn));
        for event in &buffered {
            Self::record(store, &mut recording, event);
        }
        *state = TapState::Recording(recording);
    }

    /// The turn will not be recorded (its start was refused).
    pub fn discard(&self) {
        *self.state() = TapState::Off;
    }

    /// The files the turn changed, as it ends (before its `done`).
    pub fn changes(&self, changes: &[FileChange]) {
        let Some(store) = &self.store else { return };
        let mut state = self.state();
        let TapState::Recording(recording) = &mut *state else {
            return;
        };
        let op = Op::Changes {
            turn_id: recording.turn.turn_id.clone(),
            json: stored_changes(changes).to_string(),
        };
        Self::queue(store, recording, op);
    }

    /// Hand `op` to the writer without waiting: behind any backlog, which
    /// is drained first; what does not fit stays in the backlog (text
    /// merged into its tail).
    fn queue(store: &HistoryStore, recording: &mut Recording, op: Op) {
        let op = match recording.backlog.back_mut() {
            Some(last) => last.absorb(op),
            None => Some(op),
        };
        if let Some(op) = op {
            recording.backlog.push_back(op);
        }
        while let Some(op) = recording.backlog.pop_front() {
            match store.try_send(op) {
                Sent::Queued => {}
                Sent::Full(op) => {
                    recording.backlog.push_front(op);
                    return;
                }
                // The writer is gone (the store is closing): nothing to keep.
                Sent::Closed => {
                    recording.backlog.clear();
                    return;
                }
            }
        }
    }

    /// The turn ends: what the backlog holds goes to the writer, waiting
    /// for room if it must, and the writer commits it.
    fn finish(store: &HistoryStore, recording: &mut Recording) {
        while let Some(op) = recording.backlog.pop_front() {
            if !store.send(op) {
                recording.backlog.clear();
                return;
            }
        }
        store.flush();
    }

    fn record(store: &HistoryStore, recording: &mut Recording, event: &AgentEvent) {
        let turn_id = recording.turn.turn_id.clone();
        if event.kind == "text_delta" {
            let delta = event
                .payload
                .get("text")
                .and_then(Value::as_str)
                .unwrap_or_default();
            if recording.capped || recording.bytes + delta.len() > MAX_TURN_EVENT_BYTES {
                Self::cap(store, recording);
            } else {
                recording.bytes += delta.len();
                let op = Op::Text {
                    turn_id,
                    seq: event.seq,
                    text: delta.to_owned(),
                };
                Self::queue(store, recording, op);
            }
            return;
        }
        let essential = matches!(event.kind, "status" | "error" | "done");
        let payload = bounded(event.payload.clone()).to_string();
        if !essential
            && (recording.capped || recording.bytes + payload.len() > MAX_TURN_EVENT_BYTES)
        {
            Self::cap(store, recording);
            return;
        }
        recording.bytes += payload.len();
        let op = Op::Event {
            turn_id: turn_id.clone(),
            seq: event.seq,
            kind: event.kind,
            payload,
        };
        Self::queue(store, recording, op);
        if event.kind == "done" {
            let turn = &recording.turn;
            let op = Op::Finish {
                turn_id,
                owner: turn.owner.clone(),
                workspace_id: turn.workspace_id.clone(),
                conversation_id: turn.conversation_id.clone(),
            };
            Self::queue(store, recording, op);
            Self::finish(store, recording);
        }
    }

    fn cap(store: &HistoryStore, recording: &mut Recording) {
        if recording.capped {
            return;
        }
        recording.capped = true;
        let op = Op::Truncated {
            turn_id: recording.turn.turn_id.clone(),
        };
        Self::queue(store, recording, op);
    }
}

impl Drop for TurnTap {
    /// The tap goes away (the turn ended, or the app is quitting): what it
    /// still holds is written, so an interrupted turn keeps its text.
    fn drop(&mut self) {
        let Some(store) = &self.store else { return };
        if let TapState::Recording(recording) = &mut *self.state() {
            Self::finish(store, recording);
        }
    }
}

impl EventEmitter for TurnTap {
    fn emit(&self, event: AgentEvent) {
        if let Some(store) = &self.store {
            let mut state = self.state();
            match &mut *state {
                TapState::Buffering(buffered) => buffered.push(event.clone()),
                TapState::Recording(recording) => Self::record(store, recording, &event),
                TapState::Off => {}
            }
        }
        self.inner.emit(event);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::VecEmitter;
    use rusqlite::OptionalExtension as _;
    use serde_json::json;

    fn owner(user_id: i64) -> Owner {
        Owner {
            origin: "https://elitea.example".into(),
            user_id,
        }
    }

    fn new_turn(owner: Owner, turn_id: &str, conversation: &str) -> NewTurn {
        NewTurn {
            owner,
            workspace_id: "w1".into(),
            conversation_id: conversation.into(),
            conversation_uuid: Some(format!("uuid-{conversation}")),
            turn_id: turn_id.into(),
            prompt: "Fix the build".into(),
            mentions: vec!["src/main.rs".into()],
            started_at: now_ms(),
        }
    }

    fn event(turn_id: &str, seq: u64, kind: &'static str, payload: Value) -> AgentEvent {
        AgentEvent {
            turn_id: turn_id.into(),
            seq,
            kind,
            payload,
        }
    }

    fn store() -> (tempfile::TempDir, Arc<HistoryStore>) {
        let root = tempfile::tempdir().unwrap();
        let store = HistoryStore::open(&root.path().join("ai.elitea.desktop")).unwrap();
        (root, Arc::new(store))
    }

    /// Play a whole turn through a tap; returns what the window received.
    fn play(store: &Arc<HistoryStore>, turn: NewTurn) -> Vec<AgentEvent> {
        let window = Arc::new(VecEmitter::default());
        let tap = TurnTap::new(window.clone(), Some(store.clone()));
        let id = turn.turn_id.clone();
        tap.emit(event(&id, 0, "status", json!({"phase": "resolving"})));
        tap.activate(turn);
        tap.emit(event(&id, 1, "status", json!({"phase": "running"})));
        tap.emit(event(&id, 2, "text_delta", json!({"text": "Hel"})));
        tap.emit(event(&id, 3, "text_delta", json!({"text": "lo"})));
        tap.emit(event(
            &id,
            4,
            "tool_call",
            json!({"call_id": "c1", "tool": "write_file", "args_summary": "a.txt", "remote": false}),
        ));
        tap.emit(event(
            &id,
            5,
            "tool_result",
            json!({"call_id": "c1", "ok": true, "summary": "x".repeat(MAX_EVENT_STRING * 2), "truncated": true}),
        ));
        tap.emit(event(&id, 6, "text_delta", json!({"text": "Done."})));
        tap.changes(&[FileChange {
            path: "a.txt".into(),
            status: "added",
            added: 1,
            removed: 0,
            diff: "+hi".into(),
        }]);
        tap.emit(event(&id, 7, "status", json!({"phase": "done"})));
        tap.emit(event(
            &id,
            8,
            "done",
            json!({"committed": true, "conversation_id": "42", "message_ids": [], "changed_files": 1}),
        ));
        window.all()
    }

    #[test]
    fn a_turn_round_trips_and_replays_as_it_was_emitted() {
        let (_root, store) = store();
        let sent = play(&store, new_turn(owner(1), "t1", "42"));
        assert_eq!(sent.len(), 9, "every event reaches the window");

        let turns = store.thread(&owner(1), "w1", "42").unwrap();
        assert_eq!(turns.len(), 1);
        let turn = &turns[0];
        assert_eq!(turn.prompt, "Fix the build");
        assert_eq!(turn.mentions, ["src/main.rs"]);
        assert_eq!(turn.state, "done");
        assert!(turn.finished_at.is_some());
        assert_eq!(turn.changes.as_ref().unwrap()[0]["path"], "a.txt");
        // The buffered `resolving` is stored; consecutive deltas are one row,
        // numbered by the last delta it holds.
        let compact: Vec<(u64, &str)> = turn
            .events
            .iter()
            .map(|e| (e.seq, e.kind.as_str()))
            .collect();
        assert_eq!(
            compact,
            [
                (0, "status"),
                (1, "status"),
                (3, "text_delta"),
                (4, "tool_call"),
                (5, "tool_result"),
                (6, "text_delta"),
                (7, "status"),
                (8, "done"),
            ]
        );
        assert_eq!(turn.events[2].payload["text"], "Hello");
        assert_eq!(turn.events[5].payload["text"], "Done.");
        // Folding the stored stream gives the text the window saw.
        let replayed: String = turn
            .events
            .iter()
            .filter(|e| e.kind == "text_delta")
            .map(|e| e.payload["text"].as_str().unwrap())
            .collect();
        let live: String = sent
            .iter()
            .filter(|e| e.kind == "text_delta")
            .map(|e| e.payload["text"].as_str().unwrap())
            .collect();
        assert_eq!(replayed, live);
        // A long tool result is cut, not dropped.
        let summary = turn.events[4].payload["summary"].as_str().unwrap();
        assert!(summary.len() <= MAX_EVENT_STRING + 3, "{}", summary.len());
        // The UUID finds the same thread.
        assert_eq!(store.thread(&owner(1), "w1", "uuid-42").unwrap().len(), 1);
    }

    /// The text rows of a turn as stored (seq, text), read raw: no flush.
    fn text_rows(store: &HistoryStore, turn_id: &str) -> Vec<(i64, String)> {
        store
            .with("text", |conn| {
                let mut statement = conn.prepare(
                    "SELECT seq, payload FROM events
                     WHERE turn_id = ?1 AND kind = 'text_delta' ORDER BY seq",
                )?;
                statement
                    .query_map(params![turn_id], |row| {
                        let payload: String = row.get(1)?;
                        let value: Value = serde_json::from_str(&payload).unwrap();
                        Ok((row.get(0)?, value["text"].as_str().unwrap().to_owned()))
                    })?
                    .collect()
            })
            .unwrap()
    }

    /// Wait (bounded) for the writer thread to have committed `condition`.
    fn eventually(mut condition: impl FnMut() -> bool) -> bool {
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if condition() {
                return true;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        condition()
    }

    #[test]
    fn text_is_appended_in_chunks_and_never_rewritten() {
        let (_root, store) = store();
        // Any UPDATE of a stored event fails the writer's batch (and so the test).
        store
            .with("trigger", |conn| {
                conn.execute_batch(
                    "CREATE TRIGGER no_rewrite BEFORE UPDATE ON events
                     BEGIN SELECT RAISE(FAIL, 'an event row was rewritten'); END;",
                )
            })
            .unwrap();
        let window = Arc::new(VecEmitter::default());
        let tap = TurnTap::new(window.clone(), Some(store.clone()));
        tap.activate(new_turn(owner(1), "t1", "42"));
        // 5,000 deltas, the writer made to commit every 100 (as its timer would).
        for seq in 0..5_000_u64 {
            tap.emit(event("t1", seq, "text_delta", json!({"text": "x"})));
            if seq % 100 == 99 {
                store.flush();
            }
        }
        tap.emit(event("t1", 5_000, "done", json!({"committed": true})));
        let rows = text_rows(&store, "t1");
        // Linear: 50 appended chunks of 100 bytes, not one row rewritten 5,000 times.
        assert_eq!(rows.len(), 50);
        assert!(rows.iter().all(|(_, text)| text.len() == 100));
        assert_eq!(rows[0].0, 99, "a chunk is numbered by its last delta");
        let turn = &store.thread(&owner(1), "w1", "42").unwrap()[0];
        assert_eq!(turn.state, "done");
        // Replay equals the live stream.
        let replayed: String = turn
            .events
            .iter()
            .filter(|e| e.kind == "text_delta")
            .map(|e| e.payload["text"].as_str().unwrap())
            .collect();
        let live: String = window
            .all()
            .iter()
            .filter(|e| e.kind == "text_delta")
            .map(|e| e.payload["text"].as_str().unwrap())
            .collect();
        assert_eq!(replayed, live);
        let bytes: i64 = store
            .with("bytes", |conn| {
                conn.query_row(
                    "SELECT events_bytes FROM turns WHERE turn_id = 't1'",
                    [],
                    |row| row.get(0),
                )
            })
            .unwrap();
        assert!(
            bytes < 5_000 + 50 * 16 + 100,
            "{bytes} bytes for 5,000 of text"
        );
    }

    #[test]
    fn the_unwritten_tail_is_at_most_one_batch() {
        let (_root, store) = store();
        let tap = TurnTap::new(Arc::new(VecEmitter::default()), Some(store.clone()));
        tap.activate(new_turn(owner(1), "t1", "42"));
        // Past the size threshold: written without any read or later event.
        let chunk = "b".repeat(1024);
        for seq in 0..(FLUSH_BYTES / 1024 + 1) {
            tap.emit(event(
                "t1",
                u64::try_from(seq).unwrap(),
                "text_delta",
                json!({"text": chunk}),
            ));
        }
        assert!(eventually(|| {
            text_rows(&store, "t1")
                .iter()
                .map(|(_, t)| t.len())
                .sum::<usize>()
                >= FLUSH_BYTES
        }));
        // A small tail: written by the writer's timer.
        tap.emit(event("t1", 1_000, "text_delta", json!({"text": "tail"})));
        assert!(eventually(|| text_rows(&store, "t1").last().is_some_and(
            |(seq, text)| *seq == 1_000 && text.ends_with("tail")
        )));
    }

    #[test]
    fn a_read_and_a_dropped_tap_see_the_unwritten_text() {
        let (_root, store) = store();
        let tap = TurnTap::new(Arc::new(VecEmitter::default()), Some(store.clone()));
        tap.activate(new_turn(owner(1), "t1", "42"));
        tap.emit(event("t1", 0, "text_delta", json!({"text": "Hel"})));
        // The page reopens the thread while the turn runs.
        let turns = store.thread(&owner(1), "w1", "42").unwrap();
        assert_eq!(turns[0].events[0].payload["text"], "Hel");
        tap.emit(event("t1", 1, "text_delta", json!({"text": "lo"})));
        // The app quits mid-answer.
        drop(tap);
        let again = HistoryStore::open(store.path().parent().unwrap()).unwrap();
        let turns = again.thread(&owner(1), "w1", "42").unwrap();
        let text: String = turns[0]
            .events
            .iter()
            .map(|e| e.payload["text"].as_str().unwrap())
            .collect();
        assert_eq!(text, "Hello");
        assert_eq!(turns[0].events.last().unwrap().seq, 1);
        assert_eq!(turns[0].state, "interrupted");
    }

    #[test]
    fn emit_never_waits_for_the_writer() {
        let (_root, store) = store();
        // Hold the writer's database lock so every commit stalls.
        let blocker = Connection::open(store.path()).unwrap();
        blocker.execute_batch("BEGIN IMMEDIATE;").unwrap();
        let window = Arc::new(VecEmitter::default());
        let tap = TurnTap::new(window.clone(), Some(store.clone()));
        tap.activate(new_turn(owner(1), "t1", "42"));
        let started = Instant::now();
        for seq in 0..(CHANNEL_CAPACITY as u64 * 3) {
            let kind = if seq % 2 == 0 { "text_delta" } else { "status" };
            let payload = if kind == "status" {
                json!({"phase": "running"})
            } else {
                json!({"text": "x"})
            };
            tap.emit(event("t1", seq, kind, payload));
        }
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "emit waited on SQLite ({:?})",
            started.elapsed()
        );
        assert_eq!(window.all().len(), CHANNEL_CAPACITY * 3);
        blocker.execute_batch("ROLLBACK;").unwrap();
        drop(tap);
        let turn = &store.thread(&owner(1), "w1", "42").unwrap()[0];
        assert_eq!(turn.events.len(), CHANNEL_CAPACITY * 3, "nothing was lost");
    }

    #[test]
    fn another_account_or_deployment_never_sees_the_thread() {
        let (_root, store) = store();
        play(&store, new_turn(owner(1), "t1", "42"));
        assert!(store.thread(&owner(2), "w1", "42").unwrap().is_empty());
        let elsewhere = Owner {
            origin: "https://other.example".into(),
            user_id: 1,
        };
        assert!(store.thread(&elsewhere, "w1", "42").unwrap().is_empty());
        assert!(store.thread(&owner(1), "w2", "42").unwrap().is_empty());
        assert!(store.thread(&owner(1), "w1", "43").unwrap().is_empty());
        // Nor can it delete it.
        assert_eq!(store.delete_thread(&owner(2), "w1", "42").unwrap(), 0);
        assert_eq!(store.thread(&owner(1), "w1", "42").unwrap().len(), 1);
    }

    #[test]
    fn a_refused_start_is_not_history() {
        let (_root, store) = store();
        let window = Arc::new(VecEmitter::default());
        let tap = TurnTap::new(window.clone(), Some(store.clone()));
        tap.emit(event("t1", 0, "status", json!({"phase": "resolving"})));
        tap.emit(event(
            "t1",
            1,
            "error",
            json!({"code": "workspace_busy", "message": "busy"}),
        ));
        tap.discard();
        tap.emit(event("t1", 2, "status", json!({"phase": "error"})));
        assert_eq!(window.all().len(), 3);
        assert!(store.thread(&owner(1), "w1", "42").unwrap().is_empty());
    }

    #[test]
    fn an_unfinished_turn_reads_as_interrupted() {
        let (_root, store) = store();
        let tap = TurnTap::new(Arc::new(VecEmitter::default()), Some(store.clone()));
        tap.activate(new_turn(owner(1), "t1", "42"));
        tap.emit(event("t1", 0, "status", json!({"phase": "running"})));
        // The app quits here. A new launch reads it back.
        drop(tap);
        let again = HistoryStore::open(store.path().parent().unwrap()).unwrap();
        let turns = again.thread(&owner(1), "w1", "42").unwrap();
        assert_eq!(turns[0].state, "interrupted");
        assert_eq!(turns[0].finished_at, None);
        assert_eq!(turns[0].events.len(), 1);
    }

    #[test]
    fn deleting_a_thread_or_a_workspace_removes_its_rows() {
        let (_root, store) = store();
        play(&store, new_turn(owner(1), "t1", "42"));
        play(&store, new_turn(owner(1), "t2", "43"));
        play(&store, new_turn(owner(2), "t3", "42"));
        assert_eq!(store.delete_thread(&owner(1), "w1", "42").unwrap(), 1);
        assert!(store.thread(&owner(1), "w1", "42").unwrap().is_empty());
        assert_eq!(store.thread(&owner(1), "w1", "43").unwrap().len(), 1);
        // Removing the workspace forgets it for every account, events too.
        assert_eq!(store.delete_workspace("w1").unwrap(), 2);
        assert!(store.thread(&owner(2), "w1", "42").unwrap().is_empty());
        let events: i64 = store
            .with("count", |conn| {
                conn.query_row("SELECT count(*) FROM events", [], |row| row.get(0))
            })
            .unwrap();
        assert_eq!(events, 0);
    }

    #[test]
    fn a_thread_keeps_its_newest_turns() {
        let (_root, store) = store();
        for index in 0..MAX_TURNS_PER_THREAD + 3 {
            let mut turn = new_turn(owner(1), &format!("t{index}"), "42");
            turn.started_at = i64::try_from(index).unwrap();
            play(&store, turn);
        }
        let turns = store.thread(&owner(1), "w1", "42").unwrap();
        assert_eq!(turns.len(), MAX_TURNS_PER_THREAD);
        assert_eq!(turns[0].turn_id, "t3");
    }

    #[test]
    fn a_huge_turn_keeps_its_ending() {
        let (_root, store) = store();
        let tap = TurnTap::new(Arc::new(VecEmitter::default()), Some(store.clone()));
        tap.activate(new_turn(owner(1), "t1", "42"));
        let chunk = "y".repeat(MAX_EVENT_STRING);
        let mut seq = 0;
        for _ in 0..(MAX_TURN_EVENT_BYTES / MAX_EVENT_STRING + 10) {
            tap.emit(event(
                "t1",
                seq,
                "tool_call",
                json!({"call_id": format!("c{seq}"), "tool": "t", "args_summary": chunk, "remote": false}),
            ));
            seq += 1;
        }
        tap.emit(event("t1", seq, "done", json!({"committed": true})));
        let turn = &store.thread(&owner(1), "w1", "42").unwrap()[0];
        assert!(turn.events_truncated);
        assert_eq!(turn.state, "done", "the ending is always kept");
        assert!(turn.events.len() < usize::try_from(seq).unwrap());
    }

    #[test]
    fn stored_diffs_are_bounded() {
        let big = FileChange {
            path: "big.txt".into(),
            status: "modified",
            added: 1,
            removed: 1,
            diff: "z".repeat(MAX_DIFF_BYTES * 2),
        };
        let stored = stored_changes(&std::iter::repeat_n(big, 40).collect::<Vec<_>>());
        let total: usize = stored
            .as_array()
            .unwrap()
            .iter()
            .map(|f| f["diff"].as_str().unwrap().len())
            .sum();
        assert!(total <= MAX_CHANGES_BYTES);
        assert_eq!(stored[0]["added"], 1);
        assert_eq!(stored[39]["diff"], "", "past the budget: counts only");
    }

    #[cfg(unix)]
    #[test]
    fn the_directory_and_database_are_owner_only() {
        use std::os::unix::fs::PermissionsExt as _;
        let root = tempfile::tempdir().unwrap();
        let dir = root.path().join("ai.elitea.desktop");
        fs::create_dir_all(&dir).unwrap();
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
        let store = Arc::new(HistoryStore::open(&dir).unwrap());
        play(&store, new_turn(owner(1), "t1", "42"));
        let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&dir), 0o700);
        assert_eq!(mode(store.path()), 0o600);
        let wal = dir.join(format!("{FILE_NAME}-wal"));
        if wal.exists() {
            assert_eq!(mode(&wal), 0o600);
        }
        let version: i64 = store
            .with("v", |c| {
                c.query_row("PRAGMA user_version", [], |r| r.get(0))
            })
            .unwrap();
        assert_eq!(version, SCHEMA_VERSION);
        let journal: String = store
            .with("j", |c| {
                c.query_row("PRAGMA journal_mode", [], |r| r.get(0))
            })
            .unwrap();
        assert_eq!(journal, "wal");
    }

    #[cfg(unix)]
    #[test]
    fn a_database_others_can_read_or_a_symlink_is_refused() {
        use std::os::unix::fs::PermissionsExt as _;
        let root = tempfile::tempdir().unwrap();
        let dir = root.path().join("app");
        drop(HistoryStore::open(&dir).unwrap());
        fs::set_permissions(dir.join(FILE_NAME), fs::Permissions::from_mode(0o644)).unwrap();
        let error = HistoryStore::open(&dir).err().unwrap();
        assert!(error.to_string().contains("other users"), "{error}");

        let linked = root.path().join("linked");
        fs::create_dir_all(&linked).unwrap();
        let target = root.path().join("elsewhere.sqlite");
        fs::write(&target, "").unwrap();
        std::os::unix::fs::symlink(&target, linked.join(FILE_NAME)).unwrap();
        let error = HistoryStore::open(&linked).err().unwrap();
        assert!(error.to_string().contains("symbolic link"), "{error}");
    }

    #[test]
    fn a_newer_schema_is_not_touched() {
        let root = tempfile::tempdir().unwrap();
        let dir = root.path().join("app");
        let store = HistoryStore::open(&dir).unwrap();
        store
            .with("bump", |c| c.execute_batch("PRAGMA user_version = 99;"))
            .unwrap();
        drop(store);
        let error = HistoryStore::open(&dir).err().unwrap();
        assert!(error.to_string().contains("newer version"), "{error}");
    }

    #[test]
    fn a_turn_without_a_store_still_reaches_the_window() {
        let window = Arc::new(VecEmitter::default());
        let tap = TurnTap::new(window.clone(), None);
        tap.activate(new_turn(owner(1), "t1", "42"));
        tap.emit(event("t1", 0, "status", json!({"phase": "running"})));
        assert_eq!(window.kinds(), ["status"]);
    }

    #[test]
    fn opening_the_store_again_keeps_its_rows() {
        let (_root, store) = store();
        play(&store, new_turn(owner(1), "t1", "42"));
        let dir: PathBuf = store.path().parent().unwrap().to_owned();
        drop(store);
        let again = HistoryStore::open(&dir).unwrap();
        assert_eq!(again.thread(&owner(1), "w1", "42").unwrap().len(), 1);
        assert!(
            again
                .with("x", |c| c
                    .query_row("SELECT 1 FROM turns WHERE turn_id = 't1'", [], |r| r
                        .get::<_, i64>(0))
                    .optional())
                .unwrap()
                .is_some()
        );
    }
}
