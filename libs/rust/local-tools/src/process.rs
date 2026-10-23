//! Background processes (#1223): commands the agent starts and comes back
//! to, on pipes or on a pseudo-terminal.
//!
//! One [`ProcessRegistry`] per session. A process is spawned from what
//! [`crate::sandbox::prepare`] made of its command (the same request, deny
//! list and scrubbed environment as `run_command`), in its own process
//! group, and keeps that sandbox for its whole life. Its output (stdout and
//! stderr on one pipe, as a terminal shows them, or the terminal's own
//! stream) goes into an [`OutputBuffer`] of 1 MiB that keeps the first
//! 64 KiB and the latest bytes, with offsets that only grow; a terminal
//! process also feeds a `vt100` screen, which is what the model reads of an
//! interactive program.
//!
//! Three threads per process: a reader (blocking reads of the pipe or the
//! terminal), a waiter (the child's exit) and a pump, which every 50 ms
//! hands what is new to the host's [`ProcessEvents`] (at most 16 KiB per
//! event), stops a process past its wall time, and reports the exit after
//! the last output. Stopping a process signals its group: `SIGTERM`, then
//! `SIGKILL` after [`TERM_GRACE`]. Dropping the registry (the session's
//! end) stops every process it holds.

use std::collections::VecDeque;
use std::io::{Read, Write};
use std::num::NonZeroU16;
use std::os::unix::process::CommandExt as _;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, RwLock};
use std::time::{Duration, Instant, SystemTime};

use serde::Serialize;

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;

/// Live processes one session may hold.
pub const MAX_LIVE: usize = 8;

/// Ended processes kept listed (the oldest ended one goes first).
const MAX_LISTED: usize = 32;

/// Bytes of output kept per process.
pub const BUFFER_CAP: usize = 1024 * 1024;

/// Of those, the first bytes, kept whatever follows.
pub const HEAD_CAP: usize = 64 * 1024;

/// A terminal process's size when it starts.
pub const DEFAULT_ROWS: u16 = 40;
pub const DEFAULT_COLS: u16 = 120;

/// How often the pump hands new output to the host: at most 20 events a
/// second per process.
pub const PUMP_INTERVAL: Duration = Duration::from_millis(50);

/// The most output one event carries; a process writing faster skips
/// ahead (the next event's offset is past the previous one's end).
pub const MAX_DELTA_BYTES: usize = 16 * 1024;

/// How long a stopped process gets between `SIGTERM` and `SIGKILL`.
pub const TERM_GRACE: Duration = Duration::from_secs(2);

/// How long the pump waits for the reader once the process ended (a
/// grandchild that left the group can hold the pipe).
const DRAIN_AFTER_EXIT: Duration = Duration::from_secs(2);

/// How long [`Process::write`] waits for the process to take its input
/// before it answers that the process is not reading.
pub const WRITE_TIMEOUT: Duration = Duration::from_secs(5);

/// Whether a process outlives the end of the turn that started it.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum KeepAlive {
    /// Stopped when its turn ends (unless the person pinned it).
    #[default]
    Turn,
    /// Kept until the session ends (the person approved it).
    Session,
}

/// Where a process stands.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ProcessState {
    Running,
    Exited,
    /// Ended by a signal: one the session sent (a stop) or another.
    Killed,
    /// Stopped at its wall time.
    TimedOut,
}

impl ProcessState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Running => "running",
            Self::Exited => "exited",
            Self::Killed => "killed",
            Self::TimedOut => "timed_out",
        }
    }
}

/// Which stream an output event carries.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Stream {
    /// A piped process: stdout and stderr share the pipe.
    Stdout,
    /// A terminal process's output, escape codes included.
    Pty,
}

/// A process as the host and the model see it.
#[derive(Clone, Debug, Serialize)]
#[allow(clippy::struct_excessive_bools)] // independent facts about one process
pub struct ProcessInfo {
    pub process_id: String,
    pub call_id: String,
    pub command: String,
    /// Workspace-relative.
    pub cwd: String,
    pub tty: bool,
    pub sandbox: SandboxMode,
    pub network: bool,
    pub loopback: bool,
    pub keep_alive: KeepAlive,
    pub pinned: bool,
    pub status: ProcessState,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub signal: Option<String>,
    /// Unix milliseconds.
    pub started_at: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ended_at: Option<u64>,
    /// Output so far: the offset of the next byte.
    pub bytes: u64,
}

/// New output of one process, as the pump hands it to the host.
#[derive(Clone, Copy, Debug)]
pub struct OutputDelta<'a> {
    pub process_id: &'a str,
    pub call_id: &'a str,
    pub stream: Stream,
    pub data: &'a str,
    /// The offset of `data`'s first byte.
    pub offset: u64,
}

/// Where a session's process events go (the desktop's pane and the turn's
/// tool card). Called from the processes' own threads: keep it quick.
pub trait ProcessEvents: Send + Sync {
    fn started(&self, info: &ProcessInfo);
    fn output(&self, delta: &OutputDelta<'_>);
    fn exited(&self, info: &ProcessInfo);
    /// Pinned or unpinned.
    fn updated(&self, _info: &ProcessInfo) {}
}

/// One process's output: the first [`HEAD_CAP`] bytes and the latest ones,
/// [`BUFFER_CAP`] in all, addressed by offsets that only grow.
#[derive(Debug, Default)]
pub struct OutputBuffer {
    head: Vec<u8>,
    tail: VecDeque<u8>,
    total: u64,
    head_cap: usize,
    cap: usize,
}

/// What [`OutputBuffer::read`] found.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Chunk {
    pub data: Vec<u8>,
    /// The offset of `data`'s first byte.
    pub start: u64,
    /// Where the next read continues.
    pub next: u64,
    /// Bytes between the asked offset and `start` the buffer no longer holds.
    pub dropped: u64,
}

fn to_u64(n: usize) -> u64 {
    u64::try_from(n).unwrap_or(u64::MAX)
}

fn to_usize(n: u64) -> usize {
    usize::try_from(n).unwrap_or(usize::MAX)
}

impl OutputBuffer {
    #[must_use]
    pub fn new() -> Self {
        Self::with_caps(HEAD_CAP, BUFFER_CAP)
    }

    /// A buffer keeping `head` first bytes of `cap` (tests).
    #[must_use]
    pub fn with_caps(head: usize, cap: usize) -> Self {
        Self {
            head_cap: head.min(cap),
            cap,
            ..Self::default()
        }
    }

    pub fn push(&mut self, mut bytes: &[u8]) {
        self.total += to_u64(bytes.len());
        if self.head.len() < self.head_cap {
            let take = (self.head_cap - self.head.len()).min(bytes.len());
            self.head.extend_from_slice(&bytes[..take]);
            bytes = &bytes[take..];
        }
        let room = self.cap - self.head_cap;
        if bytes.len() >= room {
            self.tail.clear();
            self.tail.extend(&bytes[bytes.len() - room..]);
            return;
        }
        let over = (self.tail.len() + bytes.len()).saturating_sub(room);
        self.tail.drain(..over);
        self.tail.extend(bytes);
    }

    /// Every byte written so far, kept or not: the next byte's offset.
    #[must_use]
    pub const fn end(&self) -> u64 {
        self.total
    }

    fn tail_start(&self) -> u64 {
        self.total - to_u64(self.tail.len())
    }

    /// Up to `max` bytes from `since` (past the end: none). Bytes the
    /// buffer dropped are skipped and counted in [`Chunk::dropped`]; the
    /// head and the tail are read one at a time.
    #[must_use]
    pub fn read(&self, since: u64, max: usize) -> Chunk {
        let since = since.min(self.total);
        let head_end = to_u64(self.head.len());
        if since < head_end {
            let from = to_usize(since);
            let to = self.head.len().min(from.saturating_add(max));
            let mut data = self.head[from..to].to_vec();
            let mut next = to_u64(to);
            // No gap between the head and the tail: go on into the tail.
            if next == head_end && self.tail_start() == head_end && data.len() < max {
                let more = (max - data.len()).min(self.tail.len());
                data.extend(self.tail.range(..more));
                next += to_u64(more);
            }
            return Chunk {
                data,
                start: since,
                next,
                dropped: 0,
            };
        }
        let tail_start = self.tail_start();
        let start = since.max(tail_start);
        let from = to_usize(start - tail_start);
        let to = self.tail.len().min(from.saturating_add(max));
        Chunk {
            data: self.tail.range(from..to).copied().collect(),
            start,
            next: tail_start + to_u64(to),
            dropped: start - since,
        }
    }

    /// The last `max` bytes (or fewer), from the tail or the head.
    #[must_use]
    pub fn last(&self, max: usize) -> Chunk {
        self.read(self.total.saturating_sub(to_u64(max)), max)
    }
}

/// `bytes` as text, without an incomplete character at its end: the length
/// of the prefix to send now (the rest waits for the next bytes).
#[must_use]
pub fn complete_prefix(bytes: &[u8]) -> usize {
    match std::str::from_utf8(bytes) {
        // An incomplete character at the very end: wait for its rest.
        Err(error) if error.error_len().is_none() && bytes.len() - error.valid_up_to() < 4 => {
            error.valid_up_to()
        }
        _ => bytes.len(),
    }
}

/// A terminal's output without its escape sequences (CSI, OSC and the
/// two-byte ones) and carriage returns before a newline: what the model
/// reads of a terminal's stream.
#[must_use]
pub fn strip_escapes(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        match c {
            '\u{1b}' => match chars.next() {
                Some('[') => {
                    // Parameters and intermediates, then one final byte.
                    for c in chars.by_ref() {
                        if ('\u{40}'..='\u{7e}').contains(&c) {
                            break;
                        }
                    }
                }
                Some(']') => {
                    // Up to BEL or ESC \.
                    while let Some(c) = chars.next() {
                        if c == '\u{7}' {
                            break;
                        }
                        if c == '\u{1b}' && chars.peek() == Some(&'\\') {
                            chars.next();
                            break;
                        }
                    }
                }
                _ => {}
            },
            '\r' if chars.peek() == Some(&'\n') => {}
            c => out.push(c),
        }
    }
    out
}

/// What [`ProcessRegistry::spawn`] starts.
#[derive(Clone, Debug)]
#[allow(clippy::struct_excessive_bools)] // independent facts about one process
pub struct SpawnSpec {
    pub call_id: String,
    /// The command as the model wrote it (what the host shows).
    pub command: String,
    /// Workspace-relative.
    pub cwd: String,
    /// The working directory's absolute path.
    pub cwd_path: PathBuf,
    /// The sandboxed program and its arguments (`sandbox::prepare`).
    pub program: PathBuf,
    pub args: Vec<String>,
    /// The whole environment (nothing is inherited).
    pub env: Vec<(String, String)>,
    pub tty: bool,
    pub sandbox: SandboxMode,
    pub network: bool,
    pub loopback: bool,
    pub keep_alive: KeepAlive,
    /// Stopped (`timed_out`) past this.
    pub max_wall: Duration,
}

#[derive(Debug)]
struct Status {
    state: ProcessState,
    exit_code: Option<i32>,
    signal: Option<String>,
    ended_at: Option<u64>,
    /// The signal the session sent to stop it, if it did.
    stopping: Option<&'static str>,
    timed_out: bool,
    reader_done: bool,
    /// The pump reported the exit (no more events).
    reported: bool,
}

type Writer = Box<dyn Write + Send>;
/// One write for the writer thread, and where it answers.
type WriteJob = (Vec<u8>, tokio::sync::oneshot::Sender<std::io::Result<()>>);
type Master = Box<dyn portable_pty::MasterPty + Send>;

/// One process.
pub struct Process {
    id: String,
    spec: SpawnSpec,
    started_at: u64,
    started: Instant,
    pinned: AtomicBool,
    group: Option<rustix::process::Pid>,
    status: Mutex<Status>,
    buffer: Mutex<OutputBuffer>,
    screen: Option<Mutex<vt100::Parser>>,
    /// The writer thread's queue (`None` once the process ended). The
    /// blocking write happens on that thread, never on the caller's: a
    /// process that does not read its input cannot stall the caller.
    input: Mutex<Option<std::sync::mpsc::SyncSender<WriteJob>>>,
    /// A write is with the writer thread (cleared when it finished).
    writing: AtomicBool,
    master: Option<Mutex<Master>>,
    /// Woken on new output and on the exit.
    changed: tokio::sync::Notify,
    /// What the pump has handed to the host.
    emitted: AtomicU64,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn unix_millis(at: SystemTime) -> u64 {
    at.duration_since(SystemTime::UNIX_EPOCH)
        .map_or(0, |since| {
            u64::try_from(since.as_millis()).unwrap_or(u64::MAX)
        })
}

fn signal_name(signal: i32) -> String {
    match signal {
        1 => "SIGHUP".to_owned(),
        2 => "SIGINT".to_owned(),
        3 => "SIGQUIT".to_owned(),
        6 => "SIGABRT".to_owned(),
        9 => "SIGKILL".to_owned(),
        11 => "SIGSEGV".to_owned(),
        13 => "SIGPIPE".to_owned(),
        15 => "SIGTERM".to_owned(),
        other => format!("signal {other}"),
    }
}

impl Process {
    #[must_use]
    pub fn id(&self) -> &str {
        &self.id
    }

    #[must_use]
    pub fn is_tty(&self) -> bool {
        self.spec.tty
    }

    #[must_use]
    pub fn sandbox(&self) -> SandboxMode {
        self.spec.sandbox
    }

    /// The workspace-relative working directory.
    #[must_use]
    pub fn cwd(&self) -> &str {
        &self.spec.cwd
    }

    #[must_use]
    pub fn is_running(&self) -> bool {
        lock(&self.status).state == ProcessState::Running
    }

    fn kept(&self) -> bool {
        self.pinned.load(Ordering::SeqCst) || self.spec.keep_alive == KeepAlive::Session
    }

    #[must_use]
    pub fn info(&self) -> ProcessInfo {
        let status = lock(&self.status);
        ProcessInfo {
            process_id: self.id.clone(),
            call_id: self.spec.call_id.clone(),
            command: self.spec.command.clone(),
            cwd: self.spec.cwd.clone(),
            tty: self.spec.tty,
            sandbox: self.spec.sandbox,
            network: self.spec.network,
            loopback: self.spec.loopback,
            keep_alive: self.spec.keep_alive,
            pinned: self.pinned.load(Ordering::SeqCst),
            status: status.state,
            exit_code: status.exit_code,
            signal: status.signal.clone(),
            started_at: self.started_at,
            ended_at: status.ended_at,
            bytes: lock(&self.buffer).end(),
        }
    }

    /// Output from `since`, at most `max` bytes.
    #[must_use]
    pub fn read(&self, since: u64, max: usize) -> Chunk {
        lock(&self.buffer).read(since, max)
    }

    /// The last `max` bytes of output.
    #[must_use]
    pub fn last(&self, max: usize) -> Chunk {
        lock(&self.buffer).last(max)
    }

    /// The offset the next output byte will have.
    #[must_use]
    pub fn end(&self) -> u64 {
        lock(&self.buffer).end()
    }

    /// A terminal process's rendered screen: its rows, trailing blanks
    /// trimmed. `None` for a piped process.
    #[must_use]
    pub fn screen(&self) -> Option<String> {
        self.screen
            .as_ref()
            .map(|parser| lock(parser).screen().contents())
    }

    /// Write `bytes` to its input (a terminal's, or stdin), waiting at
    /// most [`WRITE_TIMEOUT`] for the process to take them.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::Conflict`] when it has ended or closed its input;
    /// [`ErrorCode::Timeout`] when it does not take the bytes in time, or
    /// an earlier write is still pending.
    pub async fn write(&self, bytes: &[u8]) -> ToolResult<()> {
        self.write_within(bytes, WRITE_TIMEOUT).await
    }

    async fn write_within(&self, bytes: &[u8], timeout: Duration) -> ToolResult<()> {
        if !self.is_running() {
            return Err(ToolError::new(
                ErrorCode::Conflict,
                format!("process {} has ended", self.id),
            ));
        }
        // One write at a time: a process that stopped reading keeps the
        // writer thread on its pending write, and more would only queue.
        if self.writing.swap(true, Ordering::SeqCst) {
            return Err(ToolError::new(
                ErrorCode::Timeout,
                format!(
                    "process {} is not reading its input (an earlier write is still pending)",
                    self.id
                ),
            ));
        }
        let (answer, answered) = tokio::sync::oneshot::channel();
        let queued = lock(&self.input)
            .as_ref()
            .is_some_and(|queue| queue.try_send((bytes.to_vec(), answer)).is_ok());
        if !queued {
            self.writing.store(false, Ordering::SeqCst);
            return Err(ToolError::new(
                ErrorCode::Conflict,
                format!("process {} has closed its input", self.id),
            ));
        }
        match tokio::time::timeout(timeout, answered).await {
            Ok(Ok(Ok(()))) => Ok(()),
            Ok(Ok(Err(error))) => Err(ToolError::new(
                ErrorCode::Conflict,
                format!(
                    "process {} no longer takes input: {}",
                    self.id,
                    error.kind()
                ),
            )),
            Ok(Err(_)) => Err(ToolError::new(
                ErrorCode::Conflict,
                format!("process {} has closed its input", self.id),
            )),
            // The write stays with the writer thread, which clears
            // `writing` once the process takes it (or ends).
            Err(_) => Err(ToolError::new(
                ErrorCode::Timeout,
                format!(
                    "process {} is not reading its input (the write is still pending)",
                    self.id
                ),
            )),
        }
    }

    /// Set a terminal process's size (a piped one has none).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::InvalidArgument`] outside 1 to 1000; [`ErrorCode::Io`]
    /// when the terminal refuses.
    pub fn resize(&self, rows: u16, cols: u16) -> ToolResult<()> {
        if !(1..=1000).contains(&rows) || !(1..=1000).contains(&cols) {
            return Err(ToolError::invalid("rows and cols are 1 to 1000"));
        }
        let (Some(master), Some(screen)) = (&self.master, &self.screen) else {
            return Ok(());
        };
        lock(master)
            .resize(portable_pty::PtySize {
                rows,
                cols,
                pixel_width: 0,
                pixel_height: 0,
            })
            .map_err(|_| ToolError::new(ErrorCode::Io, "the terminal could not be resized"))?;
        if let (Some(rows), Some(cols)) = (NonZeroU16::new(rows), NonZeroU16::new(cols)) {
            lock(screen).set_size(rows, cols);
        }
        Ok(())
    }

    fn signal_group(&self, signal: rustix::process::Signal) {
        if let Some(group) = self.group {
            let _ = rustix::process::kill_process_group(group, signal);
        }
    }

    /// Send `SIGTERM` to its group (once), noting the session stopped it.
    fn terminate(&self) -> bool {
        {
            let mut status = lock(&self.status);
            if status.state != ProcessState::Running {
                return false;
            }
            status.stopping.get_or_insert("SIGTERM");
        }
        self.signal_group(rustix::process::Signal::TERM);
        true
    }

    /// `SIGKILL` its group, when it still runs.
    fn kill_hard(&self) {
        {
            let mut status = lock(&self.status);
            if status.state != ProcessState::Running {
                return;
            }
            status.stopping = Some("SIGKILL");
        }
        self.signal_group(rustix::process::Signal::KILL);
    }

    /// Stop it: `SIGTERM` to its group, `SIGKILL` after [`TERM_GRACE`] if
    /// it still runs. Returns once it ended (or was killed).
    pub async fn stop(&self) {
        if !self.terminate() {
            return;
        }
        if !self.wait_exit(TERM_GRACE).await {
            self.kill_hard();
            let _ = self.wait_exit(TERM_GRACE).await;
        }
    }

    /// Wait until it ends, at most `timeout`; whether it did.
    pub async fn wait_exit(&self, timeout: Duration) -> bool {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.changed.notified();
            if !self.is_running() {
                return true;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return !self.is_running();
            }
        }
    }

    /// Wait until it ends or writes past `offset`, at most `timeout`.
    pub async fn wait_output(&self, offset: u64, timeout: Duration) {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.changed.notified();
            if !self.is_running() || self.end() > offset {
                return;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return;
            }
        }
    }

    /// Wait until the host has heard its exit (the last event it sends),
    /// at most `timeout`; whether it has.
    pub async fn wait_reported(&self, timeout: Duration) -> bool {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.changed.notified();
            if lock(&self.status).reported {
                return true;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return lock(&self.status).reported;
            }
        }
    }

    /// Wait `timeout` (or until it ends): what `yield_ms` asks for.
    pub async fn settle(&self, timeout: Duration) {
        let _ = self.wait_exit(timeout).await;
    }
}

enum Child {
    Piped(std::process::Child),
    Pty(Box<dyn portable_pty::Child + Send + Sync>),
}

impl Child {
    fn pid(&self) -> Option<u32> {
        match self {
            Self::Piped(child) => Some(child.id()),
            Self::Pty(child) => child.process_id(),
        }
    }

    /// Block until it exits: its exit code, or the signal that ended it.
    fn wait(&mut self) -> (Option<i32>, Option<String>) {
        match self {
            Self::Piped(child) => match child.wait() {
                Ok(status) => {
                    use std::os::unix::process::ExitStatusExt as _;
                    (status.code(), status.signal().map(signal_name))
                }
                Err(_) => (None, None),
            },
            Self::Pty(child) => match child.wait() {
                Ok(status) => match status.signal() {
                    Some(signal) => (None, Some(signal.to_owned())),
                    None => (i32::try_from(status.exit_code()).ok(), None),
                },
                Err(_) => (None, None),
            },
        }
    }
}

/// The registry's shared half: what the processes' threads reach.
#[derive(Default)]
struct Shared {
    events: RwLock<Option<Arc<dyn ProcessEvents>>>,
}

impl Shared {
    fn events(&self) -> Option<Arc<dyn ProcessEvents>> {
        self.events.read().ok().and_then(|events| events.clone())
    }
}

/// One session's background processes. Dropping it stops every one.
#[derive(Default)]
pub struct ProcessRegistry {
    processes: Mutex<Vec<Arc<Process>>>,
    shared: Arc<Shared>,
}

fn new_id() -> String {
    let mut bytes = [0u8; 4];
    if getrandom(&mut bytes).is_err() {
        // A clock and the process id: unique within the session, which
        // is all an id must be (`spawn` checks).
        let nanos = SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .map_or(0, |since| since.subsec_nanos());
        bytes = (nanos ^ std::process::id()).to_le_bytes();
    }
    format!("p_{:08x}", u32::from_le_bytes(bytes))
}

/// Random bytes from the OS (`/dev/urandom`).
fn getrandom(out: &mut [u8]) -> std::io::Result<()> {
    std::fs::File::open("/dev/urandom")?.read_exact(out)
}

impl ProcessRegistry {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Where events go from now on (`None`: nowhere).
    pub fn set_events(&self, events: Option<Arc<dyn ProcessEvents>>) {
        if let Ok(mut current) = self.shared.events.write() {
            *current = events;
        }
    }

    /// Every process, oldest first.
    #[must_use]
    pub fn list(&self) -> Vec<Arc<Process>> {
        lock(&self.processes).clone()
    }

    /// One process.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::NotFound`] for an id the session does not hold.
    pub fn get(&self, id: &str) -> ToolResult<Arc<Process>> {
        lock(&self.processes)
            .iter()
            .find(|process| process.id == id)
            .cloned()
            .ok_or_else(|| {
                ToolError::new(
                    ErrorCode::NotFound,
                    format!("no process `{id}` in this session (process_list lists them)"),
                )
            })
    }

    /// How many still run.
    #[must_use]
    pub fn live(&self) -> usize {
        lock(&self.processes)
            .iter()
            .filter(|process| process.is_running())
            .count()
    }

    /// Whether another process may start now.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::TooManyProcesses`] at [`MAX_LIVE`].
    pub fn check_room(&self) -> ToolResult<()> {
        if self.live() >= MAX_LIVE {
            return Err(ToolError::new(
                ErrorCode::TooManyProcesses,
                format!(
                    "{MAX_LIVE} processes already run in this session; stop one (process_kill) \
                     before starting another"
                ),
            ));
        }
        Ok(())
    }

    /// Start `spec`: piped, or on a pseudo-terminal of
    /// [`DEFAULT_ROWS`] × [`DEFAULT_COLS`].
    ///
    /// # Errors
    ///
    /// [`ErrorCode::TooManyProcesses`], or the process cannot start.
    pub fn spawn(&self, spec: SpawnSpec) -> ToolResult<Arc<Process>> {
        self.check_room()?;
        let mut id = new_id();
        while self.get(&id).is_ok() {
            id = new_id();
        }
        let (child, reader, input, master) = if spec.tty {
            spawn_pty(&spec)?
        } else {
            spawn_piped(&spec)?
        };
        let (queue, jobs) = std::sync::mpsc::sync_channel(1);
        let group = child
            .pid()
            .and_then(|pid| i32::try_from(pid).ok())
            .and_then(rustix::process::Pid::from_raw);
        let process = Arc::new(Process {
            id,
            started_at: unix_millis(SystemTime::now()),
            started: Instant::now(),
            pinned: AtomicBool::new(false),
            group,
            status: Mutex::new(Status {
                state: ProcessState::Running,
                exit_code: None,
                signal: None,
                ended_at: None,
                stopping: None,
                timed_out: false,
                reader_done: false,
                reported: false,
            }),
            buffer: Mutex::new(OutputBuffer::new()),
            screen: spec.tty.then(|| {
                Mutex::new(vt100::Parser::new(
                    NonZeroU16::new(DEFAULT_ROWS).unwrap_or(NonZeroU16::MIN),
                    NonZeroU16::new(DEFAULT_COLS).unwrap_or(NonZeroU16::MIN),
                    0,
                ))
            }),
            input: Mutex::new(Some(queue)),
            writing: AtomicBool::new(false),
            master: master.map(Mutex::new),
            changed: tokio::sync::Notify::new(),
            emitted: AtomicU64::new(0),
            spec,
        });
        {
            let mut processes = lock(&self.processes);
            processes.push(Arc::clone(&process));
            // Ended processes past the list's size go, the oldest first.
            while processes.len() > MAX_LISTED {
                let Some(at) = processes.iter().position(|process| !process.is_running()) else {
                    break;
                };
                processes.remove(at);
            }
        }
        if let Some(events) = self.shared.events() {
            events.started(&process.info());
        }
        start_threads(&process, child, reader, (input, jobs), &self.shared);
        Ok(process)
    }

    /// Pin or unpin a process: a pinned one outlives its turn's end.
    /// Unpinning one past its turn that the agent did not keep stops it.
    ///
    /// # Errors
    ///
    /// [`ErrorCode::NotFound`].
    pub async fn pin(&self, id: &str, pinned: bool, turn_running: bool) -> ToolResult<()> {
        let process = self.get(id)?;
        process.pinned.store(pinned, Ordering::SeqCst);
        if let Some(events) = self.shared.events() {
            events.updated(&process.info());
        }
        if !pinned && !turn_running && !process.kept() {
            process.stop().await;
        }
        Ok(())
    }

    /// A turn ended: stop every process that is neither pinned nor kept.
    pub async fn end_turn(&self) {
        let ending: Vec<Arc<Process>> = self
            .list()
            .into_iter()
            .filter(|process| process.is_running() && !process.kept())
            .collect();
        stop_all(&ending).await;
        // The turn's channel closes next: let each exit reach it first.
        for process in &ending {
            process
                .wait_reported(DRAIN_AFTER_EXIT + 2 * PUMP_INTERVAL)
                .await;
        }
    }

    /// Stop every process (the session ends, a handoff starts).
    pub async fn stop_all(&self) {
        stop_all(&self.list()).await;
    }

    /// The first half of [`Self::stop_all`] from synchronous code (the app
    /// quits): `SIGTERM` every running process now, and return them for
    /// [`kill_left`] once the caller's grace is over (one grace shared
    /// with whatever else the caller stops).
    pub fn terminate_all(&self) -> Vec<Arc<Process>> {
        self.list()
            .into_iter()
            .filter(|process| process.terminate())
            .collect()
    }
}

/// The second half of [`ProcessRegistry::terminate_all`]: `SIGKILL`
/// whatever of `stopping` still runs.
pub fn kill_left(stopping: &[Arc<Process>]) {
    for process in stopping {
        process.kill_hard();
    }
}

async fn stop_all(processes: &[Arc<Process>]) {
    let stopping: Vec<&Arc<Process>> = processes
        .iter()
        .filter(|process| process.terminate())
        .collect();
    let deadline = tokio::time::Instant::now() + TERM_GRACE;
    for process in &stopping {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        let _ = process.wait_exit(left).await;
    }
    for process in stopping {
        if process.is_running() {
            process.kill_hard();
            let _ = process.wait_exit(TERM_GRACE).await;
        }
    }
}

impl Drop for ProcessRegistry {
    fn drop(&mut self) {
        // The session is gone: `SIGTERM` every group now and `SIGKILL`
        // what is left after the grace, off this thread.
        let stopping: Vec<Arc<Process>> = lock(&self.processes)
            .iter()
            .filter(|process| process.terminate())
            .cloned()
            .collect();
        if stopping.is_empty() {
            return;
        }
        let _ = std::thread::Builder::new()
            .name("elitea-process-stop".to_owned())
            .spawn(move || {
                let deadline = Instant::now() + TERM_GRACE;
                while stopping.iter().any(|process| process.is_running())
                    && Instant::now() < deadline
                {
                    std::thread::sleep(Duration::from_millis(20));
                }
                for process in &stopping {
                    process.kill_hard();
                }
            });
    }
}

type Spawned = (Child, Box<dyn Read + Send>, Writer, Option<Master>);

fn spawn_failed(spec: &SpawnSpec, error: &dyn std::fmt::Display) -> ToolError {
    ToolError::new(
        ErrorCode::Io,
        format!("cannot start `{}`: {error}", spec.command),
    )
}

/// Stdout and stderr on one pipe, stdin piped, in a new process group.
fn spawn_piped(spec: &SpawnSpec) -> ToolResult<Spawned> {
    let (reader, writer) = std::io::pipe().map_err(|error| spawn_failed(spec, &error))?;
    let stderr = writer
        .try_clone()
        .map_err(|error| spawn_failed(spec, &error))?;
    let mut command = std::process::Command::new(&spec.program);
    command
        .args(&spec.args)
        .current_dir(&spec.cwd_path)
        .env_clear()
        .envs(spec.env.iter().map(|(name, value)| (name, value)))
        .stdin(Stdio::piped())
        .stdout(writer)
        .stderr(stderr)
        .process_group(0);
    let mut child = command
        .spawn()
        .map_err(|error| spawn_failed(spec, &error))?;
    // Our copies of the pipe's writing end go with the command: the reader
    // sees the end once the process group closes its own.
    drop(command);
    let input = child
        .stdin
        .take()
        .ok_or_else(|| ToolError::new(ErrorCode::Io, "no stdin"))?;
    Ok((Child::Piped(child), Box::new(reader), Box::new(input), None))
}

/// On a new pseudo-terminal: the program is its session's leader (and so
/// its process group's), with the terminal as its controlling terminal.
fn spawn_pty(spec: &SpawnSpec) -> ToolResult<Spawned> {
    let pair = portable_pty::native_pty_system()
        .openpty(portable_pty::PtySize {
            rows: DEFAULT_ROWS,
            cols: DEFAULT_COLS,
            pixel_width: 0,
            pixel_height: 0,
        })
        .map_err(|error| spawn_failed(spec, &error))?;
    let mut command = portable_pty::CommandBuilder::new(&spec.program);
    command.args(&spec.args);
    command.cwd(&spec.cwd_path);
    command.env_clear();
    for (name, value) in &spec.env {
        command.env(name, value);
    }
    let child = pair
        .slave
        .spawn_command(command)
        .map_err(|error| spawn_failed(spec, &error))?;
    // Only the child holds the terminal's other end: its exit ends the
    // reader.
    drop(pair.slave);
    let reader = pair
        .master
        .try_clone_reader()
        .map_err(|error| spawn_failed(spec, &error))?;
    let input = pair
        .master
        .take_writer()
        .map_err(|error| spawn_failed(spec, &error))?;
    Ok((Child::Pty(child), reader, input, Some(pair.master)))
}

fn start_threads(
    process: &Arc<Process>,
    mut child: Child,
    mut reader: Box<dyn Read + Send>,
    (mut input, jobs): (Writer, std::sync::mpsc::Receiver<WriteJob>),
    shared: &Arc<Shared>,
) {
    let reading = Arc::clone(process);
    let read = move || {
        let mut chunk = [0u8; 8192];
        loop {
            match reader.read(&mut chunk) {
                Ok(0) => break,
                Ok(read) => {
                    lock(&reading.buffer).push(&chunk[..read]);
                    if let Some(screen) = &reading.screen {
                        lock(screen).process(&chunk[..read]);
                    }
                    reading.changed.notify_waiters();
                }
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {}
                // A closed terminal answers EIO.
                Err(_) => break,
            }
        }
        lock(&reading.status).reader_done = true;
        reading.changed.notify_waiters();
    };
    let waiting = Arc::clone(process);
    let wait = move || {
        let (code, signal) = child.wait();
        // Whatever the leader left in its group (a server's workers) goes
        // with it; the group id stays ours until then.
        waiting.signal_group(rustix::process::Signal::KILL);
        {
            let mut status = lock(&waiting.status);
            status.ended_at = Some(unix_millis(SystemTime::now()));
            if status.timed_out {
                status.state = ProcessState::TimedOut;
                status.signal = signal;
            } else if let Some(sent) = status.stopping {
                status.state = ProcessState::Killed;
                status.exit_code = code;
                // The session's own name for it (a terminal child reports
                // the signal's description).
                status.signal = Some(sent.to_owned());
                let _ = signal;
            } else if let Some(signal) = signal {
                status.state = ProcessState::Killed;
                status.signal = Some(signal);
            } else {
                status.state = ProcessState::Exited;
                status.exit_code = code;
            }
        }
        // It takes no more input: the writer thread ends with its queue.
        lock(&waiting.input).take();
        waiting.changed.notify_waiters();
    };
    let feeding = Arc::clone(process);
    let write = move || {
        for (bytes, answer) in jobs {
            let written = input.write_all(&bytes).and_then(|()| input.flush());
            feeding.writing.store(false, Ordering::SeqCst);
            // The caller may have stopped waiting (its timeout).
            let _ = answer.send(written);
        }
    };
    let pumping = Arc::clone(process);
    let shared = Arc::clone(shared);
    let pump = move || pump(&pumping, &shared);
    for (name, work) in [
        (
            "elitea-process-read",
            Box::new(read) as Box<dyn FnOnce() + Send>,
        ),
        ("elitea-process-wait", Box::new(wait)),
        ("elitea-process-write", Box::new(write)),
        ("elitea-process-pump", Box::new(pump)),
    ] {
        if let Err(error) = std::thread::Builder::new()
            .name(name.to_owned())
            .spawn(work)
        {
            // Without its threads the process cannot be followed: stop it.
            tracing::warn!(%error, "a background process thread could not start");
            process.kill_hard();
        }
    }
}

/// Hand what is new to the host, every [`PUMP_INTERVAL`], until the exit
/// is reported.
fn pump(process: &Arc<Process>, shared: &Shared) {
    let stream = if process.spec.tty {
        Stream::Pty
    } else {
        Stream::Stdout
    };
    let mut ended_since: Option<Instant> = None;
    loop {
        std::thread::sleep(PUMP_INTERVAL);
        let (running, reader_done) = {
            let status = lock(&process.status);
            (status.state == ProcessState::Running, status.reader_done)
        };
        if running && process.started.elapsed() > process.spec.max_wall {
            lock(&process.status).timed_out = true;
            process.signal_group(rustix::process::Signal::KILL);
        }
        let events = shared.events();
        let emitted = process.emitted.load(Ordering::SeqCst);
        let end = process.end();
        if end > emitted {
            // Falling behind: skip to the latest bytes one event holds.
            let from = emitted.max(end.saturating_sub(to_u64(MAX_DELTA_BYTES)));
            let chunk = process.read(from, MAX_DELTA_BYTES);
            let finishing = !running && reader_done;
            let keep = if finishing {
                chunk.data.len()
            } else {
                complete_prefix(&chunk.data)
            };
            if keep > 0 {
                let data = String::from_utf8_lossy(&chunk.data[..keep]);
                if let Some(events) = &events {
                    events.output(&OutputDelta {
                        process_id: &process.id,
                        call_id: &process.spec.call_id,
                        stream,
                        data: &data,
                        offset: chunk.start,
                    });
                }
                process
                    .emitted
                    .store(chunk.start + to_u64(keep), Ordering::SeqCst);
                continue;
            }
        }
        if running {
            continue;
        }
        let since = *ended_since.get_or_insert_with(Instant::now);
        if reader_done || since.elapsed() > DRAIN_AFTER_EXIT {
            if process.end() > process.emitted.load(Ordering::SeqCst) && !reader_done {
                // The reader still holds a stream something escaped with:
                // what it read so far was sent; the rest is not waited for.
                tracing::debug!(process = %process.id, "a background process's output was cut off");
            }
            lock(&process.status).reported = true;
            if let Some(events) = &events {
                events.exited(&process.info());
            }
            process.changed.notify_waiters();
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};
    use std::time::Duration;

    use super::{
        Chunk, KeepAlive, OutputBuffer, OutputDelta, ProcessEvents, ProcessInfo, ProcessRegistry,
        ProcessState, SpawnSpec, complete_prefix, strip_escapes,
    };
    use crate::policy::SandboxMode;

    #[test]
    fn head_tail_buffer_keeps_offsets_and_reports_dropped() {
        let mut buffer = OutputBuffer::with_caps(4, 10);
        buffer.push(b"abcdef");
        assert_eq!(buffer.end(), 6);
        assert_eq!(
            buffer.read(0, 100),
            Chunk {
                data: b"abcdef".to_vec(),
                start: 0,
                next: 6,
                dropped: 0
            }
        );
        assert_eq!(buffer.read(2, 3).data, b"cde");
        assert_eq!(buffer.read(6, 10).data, b"");
        // Past the cap: the head stays, the middle goes.
        buffer.push(b"ghijklmnop");
        assert_eq!(buffer.end(), 16);
        let head = buffer.read(0, 100);
        assert_eq!(
            (head.data.as_slice(), head.next, head.dropped),
            (&b"abcd"[..], 4, 0)
        );
        let after = buffer.read(head.next, 100);
        assert_eq!(after.data, b"klmnop");
        assert_eq!((after.start, after.next, after.dropped), (10, 16, 6));
        // `since` within the kept tail reads only what follows it.
        assert_eq!(buffer.read(14, 100).data, b"op");
        assert_eq!(buffer.last(3).data, b"nop");
        // One push larger than the whole tail keeps its last bytes.
        buffer.push(&[b'z'; 50]);
        assert_eq!(buffer.end(), 66);
        let tail = buffer.read(4, 100);
        assert_eq!((tail.data.len(), tail.start, tail.dropped), (6, 60, 56));
    }

    #[test]
    fn text_is_cut_only_after_whole_characters_and_escapes_are_stripped() {
        let euro = "€".as_bytes();
        assert_eq!(complete_prefix(b"ab"), 2);
        assert_eq!(complete_prefix(&[b'a', euro[0], euro[1]]), 1);
        assert_eq!(complete_prefix(&[b'a', 0xff, b'b']), 3);
        assert_eq!(
            strip_escapes("\u{1b}[1;32mok\u{1b}[0m\r\n\u{1b}]0;title\u{7}>>> "),
            "ok\n>>> "
        );
    }

    #[derive(Default)]
    struct Recorded(Mutex<Vec<String>>);

    impl ProcessEvents for Recorded {
        fn started(&self, info: &ProcessInfo) {
            self.0
                .lock()
                .expect("lock")
                .push(format!("started {}", info.process_id));
        }
        fn output(&self, delta: &OutputDelta<'_>) {
            self.0
                .lock()
                .expect("lock")
                .push(format!("output {} {}", delta.offset, delta.data));
        }
        fn exited(&self, info: &ProcessInfo) {
            self.0
                .lock()
                .expect("lock")
                .push(format!("exited {}", info.status.as_str()));
        }
    }

    /// An unsandboxed spec (the session wraps the real ones).
    fn spec(script: &str, keep_alive: KeepAlive) -> SpawnSpec {
        SpawnSpec {
            call_id: "call".to_owned(),
            command: script.to_owned(),
            cwd: ".".to_owned(),
            cwd_path: std::env::temp_dir(),
            program: "/bin/sh".into(),
            args: vec!["-c".to_owned(), script.to_owned()],
            env: vec![("PATH".to_owned(), "/usr/bin:/bin".to_owned())],
            tty: false,
            sandbox: SandboxMode::WorkspaceWrite,
            network: false,
            loopback: false,
            keep_alive,
            max_wall: Duration::from_mins(5),
        }
    }

    #[tokio::test]
    async fn processes_die_at_turn_end_unless_kept() {
        let registry = ProcessRegistry::new();
        let plain = registry
            .spawn(spec("sleep 30", KeepAlive::Turn))
            .expect("plain");
        let kept = registry
            .spawn(spec("sleep 30", KeepAlive::Session))
            .expect("kept");
        let pinned = registry
            .spawn(spec("sleep 30", KeepAlive::Turn))
            .expect("pinned");
        registry.pin(pinned.id(), true, true).await.expect("pin");
        registry.end_turn().await;
        assert!(!plain.is_running(), "the turn's own process is stopped");
        assert_eq!(plain.info().status, ProcessState::Killed);
        assert_eq!(plain.info().signal.as_deref(), Some("SIGTERM"));
        assert!(kept.is_running() && pinned.is_running());
        // Unpinned after its turn: it goes too.
        registry
            .pin(pinned.id(), false, false)
            .await
            .expect("unpin");
        assert!(!pinned.is_running());
        assert!(kept.is_running());
        registry.stop_all().await;
        assert!(!kept.is_running());
    }

    #[tokio::test]
    async fn session_end_kills_pinned_processes_too() {
        let registry = ProcessRegistry::new();
        let pinned = registry
            .spawn(spec("sleep 30", KeepAlive::Session))
            .expect("spawn");
        registry.pin(pinned.id(), true, true).await.expect("pin");
        // A process that ignores SIGTERM is killed after the grace.
        let stubborn = registry
            .spawn(spec("trap '' TERM; sleep 30", KeepAlive::Session))
            .expect("spawn");
        tokio::time::sleep(Duration::from_millis(200)).await;
        drop(registry);
        assert!(pinned.wait_exit(Duration::from_secs(1)).await);
        assert!(stubborn.wait_exit(Duration::from_secs(5)).await);
        assert_eq!(stubborn.info().signal.as_deref(), Some("SIGKILL"));
    }

    #[tokio::test]
    async fn output_is_pumped_in_order_and_the_exit_comes_last() {
        let registry = ProcessRegistry::new();
        let events = Arc::new(Recorded::default());
        registry.set_events(Some(events.clone()));
        let process = registry
            .spawn(spec("echo one; echo two >&2; exit 3", KeepAlive::Turn))
            .expect("spawn");
        assert!(process.wait_exit(Duration::from_secs(5)).await);
        for _ in 0..100 {
            if process.info().status != ProcessState::Running
                && events
                    .0
                    .lock()
                    .expect("lock")
                    .last()
                    .is_some_and(|last| last.starts_with("exited"))
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        let seen = events.0.lock().expect("lock").clone();
        assert_eq!(
            seen.first().map(String::as_str),
            Some(format!("started {}", process.id()).as_str())
        );
        assert_eq!(seen.last().map(String::as_str), Some("exited exited"));
        let text: String = seen
            .iter()
            .filter_map(|line| line.strip_prefix("output "))
            .map(|line| line.split_once(' ').map_or("", |(_, data)| data))
            .collect();
        assert_eq!(text, "one\ntwo\n", "stdout and stderr share the pipe");
        assert_eq!(process.info().exit_code, Some(3));
        // Writing to an ended process is refused.
        assert!(process.write(b"x").await.is_err());
    }

    #[tokio::test]
    async fn stdin_reaches_a_piped_process_and_the_live_cap_holds() {
        let registry = ProcessRegistry::new();
        let cat = registry
            .spawn(spec("read line; echo got $line", KeepAlive::Turn))
            .expect("spawn");
        cat.write(b"hello\n").await.expect("write");
        assert!(cat.wait_exit(Duration::from_secs(5)).await);
        cat.wait_output(0, Duration::from_secs(1)).await;
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(cat.read(0, 100).data, b"got hello\n");
        let mut held = Vec::new();
        for _ in 0..super::MAX_LIVE {
            held.push(
                registry
                    .spawn(spec("sleep 30", KeepAlive::Turn))
                    .expect("spawn"),
            );
        }
        let refused = registry
            .spawn(spec("sleep 30", KeepAlive::Turn))
            .err()
            .expect("refused");
        assert_eq!(refused.code().as_str(), "local_tools.too_many_processes");
        registry.stop_all().await;
        assert!(held.iter().all(|process| !process.is_running()));
    }

    #[tokio::test]
    async fn a_process_that_does_not_read_cannot_stall_a_write_or_its_stop() {
        let registry = ProcessRegistry::new();
        let deaf = registry
            .spawn(spec("sleep 30", KeepAlive::Turn))
            .expect("spawn");
        // The pipe's buffer fills, then a write waits only its timeout.
        let chunk = vec![b'x'; 64 * 1024];
        let started = std::time::Instant::now();
        let mut refused = None;
        for _ in 0..8 {
            if let Err(error) = deaf.write_within(&chunk, Duration::from_millis(200)).await {
                refused = Some(error);
                break;
            }
        }
        let refused = refused.expect("a full pipe refuses the write");
        assert_eq!(refused.code().as_str(), "local_tools.timeout");
        assert!(started.elapsed() < Duration::from_secs(5));
        // While that write is pending, the next is refused at once.
        let again = std::time::Instant::now();
        assert!(deaf.write(b"y").await.is_err());
        assert!(again.elapsed() < Duration::from_secs(1));
        // And the stuck write does not hold up its stop.
        let stopping = std::time::Instant::now();
        deaf.stop().await;
        assert!(!deaf.is_running());
        assert!(stopping.elapsed() < Duration::from_secs(3));
    }
}
