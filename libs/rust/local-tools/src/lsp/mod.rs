//! Language-server diagnostics after edits (#1236): `write_file`,
//! `edit_file`, `multi_edit` and `apply_patch` results gain a
//! `diagnostics` field with the errors the edit introduced, and the
//! local-only `diagnostics` tool reads what the servers report now.
//!
//! **Servers.** One per language ([`Language`]: rust-analyzer,
//! typescript-language-server, pyright, gopls), found on the host's `PATH`
//! (or given by the host, [`LspSettings::programs`]), never downloaded. A
//! language is served when a marker file is at the workspace root
//! (`Cargo.toml`, `package.json`, …). A server starts on the first edit of
//! one of its files in the session, under the fixed sandbox of
//! `launch` (read-only, no network, the deny list hidden; only where that
//! sandbox is fully enforced), stops after [`LspSettings::idle`] without
//! use and with the session; at most [`MAX_SERVERS`] run per workspace
//! (the one used least recently makes room). Nothing starts while the
//! policy turns commands off: a language server runs the project's code.
//! A server that is missing or cannot start is reported once, in the next
//! edit's result; the edit itself never fails because of diagnostics.
//!
//! **Sync.** The text a server is given is read through the confined
//! [`Workspace`] after the write, never by absolute path, and a path the
//! session denies is never opened in a server (the server could read it
//! from disk itself only through its sandbox, which hides it). The first
//! edit of a file opens it with its old text and waits for that text's
//! diagnostics: what was already wrong is not reported again. Then the new
//! text is sent (`didChange`, `didSave`) and its diagnostics are awaited,
//! up to the language's wait.
//!
//! **Output.** Only new errors (warnings too while at most
//! [`MAX_NEW_WARNINGS`] are new), at most [`MAX_REPORTED`] of them in
//! [`MAX_REPORT_BYTES`]; the server's messages are data, cut short and
//! stripped of control characters.

mod client;
mod launch;
mod registry;
#[cfg(test)]
mod tests;

use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard, RwLock, Weak};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use self::client::{Diag, Severity};
pub use self::launch::{admissible, server_environment};
pub use self::registry::{Language, language_of};
use crate::error::{ToolError, ToolResult};
use crate::session::LocalSession;
use crate::workspace::{Intent, Workspace, WsPath};

/// The most servers one workspace runs at once.
pub const MAX_SERVERS: usize = 2;

/// How long a server may sit unused before it is stopped.
pub const IDLE: Duration = Duration::from_mins(10);

/// The most entries, and bytes of them, an edit's `diagnostics` carries.
pub const MAX_REPORTED: usize = 20;
pub const MAX_REPORT_BYTES: usize = 4 * 1024;

/// New warnings are reported only while there are at most this many.
pub const MAX_NEW_WARNINGS: usize = 5;

/// The largest file given to a server.
pub const MAX_DOCUMENT_BYTES: u64 = 2 * 1024 * 1024;

/// A server that crashed is started again this many times in a session.
const MAX_RESTARTS: u32 = 2;

/// After an answer, a later one (rust-analyzer's own check, then cargo's)
/// is waited for this long.
const SETTLE: Duration = Duration::from_millis(150);

/// The longest wait a host may set.
pub const MAX_WAIT: Duration = Duration::from_secs(30);

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// A server program the host names instead of the `PATH` lookup.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct ServerProgram {
    /// An absolute path, outside the workspace.
    pub program: PathBuf,
    #[serde(default)]
    pub args: Vec<String>,
}

/// The host's settings for a session's language servers (from its own data,
/// never from the workspace).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LspSettings {
    /// Off by default: the host turns diagnostics on (the desktop does,
    /// per workspace, unless the person turned them off).
    pub enabled: bool,
    /// How long an edit waits for a language's diagnostics, every time,
    /// instead of the language's defaults ([`Language::default_wait`],
    /// [`Language::first_wait`]); at most [`MAX_WAIT`].
    pub waits: BTreeMap<Language, Duration>,
    /// Programs instead of the `PATH` lookup.
    pub programs: BTreeMap<Language, ServerProgram>,
    /// An unused server stops after this long.
    pub idle: Duration,
}

impl Default for LspSettings {
    fn default() -> Self {
        Self {
            enabled: false,
            waits: BTreeMap::new(),
            programs: BTreeMap::new(),
            idle: IDLE,
        }
    }
}

impl LspSettings {
    /// An edit's wait: the host's for the language, else its default, which
    /// is longer the first time after a start.
    fn wait(&self, language: Language, first: bool) -> Duration {
        match self.waits.get(&language) {
            Some(wait) => (*wait).min(MAX_WAIT),
            None if first => language.first_wait(),
            None => language.default_wait(),
        }
    }
}

/// Where a language's server is (the `lsp_status` event).
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum LspState {
    Starting,
    Ready,
    /// Missing, refused by the sandbox or the policy, or failed to start.
    Unavailable,
    /// Its process ended on its own.
    Crashed,
    /// Stopped: idle, room for another, settings changed, session end.
    Stopped,
}

/// One language's server state.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct LspStatus {
    pub language: Language,
    pub state: LspState,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

/// Where the host hears about servers starting and stopping.
pub trait LspEvents: Send + Sync {
    fn status(&self, status: &LspStatus);
}

struct Doc {
    /// The absolute path, as diagnostics are matched.
    key: String,
    uri: String,
    version: i32,
}

struct Server {
    id: u64,
    /// How many times this language's server crashed before this one.
    restarts: u32,
    process: launch::Process,
    docs: Mutex<HashMap<WsPath, Doc>>,
    last_used: Mutex<Instant>,
    /// Whether its first (longer) wait is still to come.
    fresh: Mutex<bool>,
    /// One sync at a time.
    sync: tokio::sync::Mutex<()>,
}

impl Server {
    fn touch(&self) {
        *lock(&self.last_used) = Instant::now();
    }

    fn take_first(&self) -> bool {
        std::mem::replace(&mut *lock(&self.fresh), false)
    }
}

enum Slot {
    Ready(Arc<Server>),
    Unavailable(String),
    Crashed(u32),
}

#[derive(Default)]
struct Inner {
    settings: Mutex<LspSettings>,
    slots: Mutex<BTreeMap<Language, Slot>>,
    /// Languages whose unavailability an edit result has reported.
    reported: Mutex<BTreeSet<Language>>,
    /// The `PATH` lookups, done once per session.
    found: Mutex<HashMap<Language, Option<PathBuf>>>,
    /// Every file synced in the session (what `diagnostics` without a path
    /// reads).
    synced: Mutex<BTreeSet<WsPath>>,
    events: RwLock<Option<Arc<dyn LspEvents>>>,
    starting: tokio::sync::Mutex<()>,
    next_id: AtomicU64,
}

impl Inner {
    fn emit(&self, language: Language, state: LspState, message: Option<String>) {
        let events = self
            .events
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone();
        if let Some(events) = events {
            events.status(&LspStatus {
                language,
                state,
                message,
            });
        }
    }

    /// Stop `language`'s server if it has been idle (the reaper's check);
    /// whether the reaper is done with `id`.
    fn reap(&self, language: Language, id: u64) -> bool {
        let idle = lock(&self.settings).idle;
        let mut slots = lock(&self.slots);
        let Some(Slot::Ready(server)) = slots.get(&language) else {
            return true;
        };
        if server.id != id {
            return true;
        }
        if server.process.exited() {
            return true;
        }
        if lock(&server.last_used).elapsed() < idle {
            return false;
        }
        slots.remove(&language);
        drop(slots);
        self.emit(language, LspState::Stopped, Some("idle".to_owned()));
        true
    }
}

/// The text of the files an edit is about to change that no server has
/// open yet: their diagnostics before the edit are what is not reported
/// again.
#[derive(Default)]
pub struct Before(HashMap<WsPath, String>);

/// One file an edit changed.
pub struct Changed {
    pub path: WsPath,
    pub deleted: bool,
}

/// A session's language servers. Dropping it stops every one.
#[derive(Default)]
pub struct LanguageServers {
    inner: Arc<Inner>,
}

impl LanguageServers {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Take the host's settings: servers of a language whose program
    /// changed stop, every server stops when diagnostics are turned off,
    /// and what was unavailable is tried again.
    pub fn configure(&self, settings: LspSettings) {
        let previous = std::mem::replace(&mut *lock(&self.inner.settings), settings);
        let settings = self.settings();
        if previous == settings {
            return;
        }
        let mut stopped = Vec::new();
        {
            let mut slots = lock(&self.inner.slots);
            slots.retain(|language, slot| {
                let keep = settings.enabled
                    && previous.programs.get(language) == settings.programs.get(language)
                    && !matches!(slot, Slot::Unavailable(_));
                if !keep && matches!(slot, Slot::Ready(_)) {
                    stopped.push(*language);
                }
                keep
            });
        }
        lock(&self.inner.reported).clear();
        lock(&self.inner.found).clear();
        for language in stopped {
            self.inner.emit(
                language,
                LspState::Stopped,
                Some("settings changed".to_owned()),
            );
        }
    }

    #[must_use]
    pub fn settings(&self) -> LspSettings {
        lock(&self.inner.settings).clone()
    }

    /// Where the host hears about servers; `None`: nowhere.
    pub fn set_events(&self, events: Option<Arc<dyn LspEvents>>) {
        *self
            .inner
            .events
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = events;
    }

    /// Every language that has a server, or had one, in this session.
    #[must_use]
    pub fn status(&self) -> Vec<LspStatus> {
        lock(&self.inner.slots)
            .iter()
            .map(|(language, slot)| {
                let (state, message) = match slot {
                    Slot::Ready(server) if server.process.exited() => (LspState::Crashed, None),
                    Slot::Ready(_) => (LspState::Ready, None),
                    Slot::Unavailable(why) => (LspState::Unavailable, Some(why.clone())),
                    Slot::Crashed(_) => (LspState::Crashed, None),
                };
                LspStatus {
                    language: *language,
                    state,
                    message,
                }
            })
            .collect()
    }

    /// Stop every server (the policy changed, the session ends).
    pub fn stop_all(&self) {
        let stopped: Vec<Language> = {
            let mut slots = lock(&self.inner.slots);
            let ready = slots
                .iter()
                .filter(|(_, slot)| matches!(slot, Slot::Ready(_)))
                .map(|(language, _)| *language)
                .collect();
            slots.clear();
            ready
        };
        for language in stopped {
            self.inner.emit(language, LspState::Stopped, None);
        }
    }

    fn program(&self, language: Language) -> Option<(PathBuf, Vec<String>)> {
        if let Some(given) = lock(&self.inner.settings).programs.get(&language) {
            return Some((given.program.clone(), given.args.clone()));
        }
        let (name, args) = language.server();
        let found = lock(&self.inner.found)
            .entry(language)
            .or_insert_with(|| registry::find_on_path(name, &crate::command::search_path()))
            .clone()?;
        Some((found, args.iter().map(|arg| (*arg).to_owned()).collect()))
    }

    /// Whether the `diagnostics` tool is offered: diagnostics on, commands
    /// allowed (`shell`), and a language at the workspace root whose server
    /// exists.
    pub(crate) fn offered(&self, workspace: &Workspace, shell: bool) -> bool {
        shell
            && lock(&self.inner.settings).enabled
            && Language::ALL.iter().any(|language| {
                registry::detected(workspace, *language) && self.program(*language).is_some()
            })
    }

    /// The file a server should be told about, with its language: one with
    /// a server language whose marker is at the root, which the session
    /// does not deny.
    fn served(workspace: &Workspace, path: &WsPath) -> Option<(Language, &'static str)> {
        let (language, id) = language_of(path)?;
        (workspace.denied_by(path, Intent::Read).is_none()
            && registry::detected(workspace, language))
        .then_some((language, id))
    }

    /// Before an edit of `paths` (off the async runtime): the old text of
    /// those no server has open yet.
    pub(crate) fn before(&self, workspace: &Workspace, paths: &[WsPath]) -> Before {
        let mut out = Before::default();
        if !lock(&self.inner.settings).enabled {
            return out;
        }
        for path in paths {
            let Some((language, _)) = Self::served(workspace, path) else {
                continue;
            };
            let open = match lock(&self.inner.slots).get(&language) {
                Some(Slot::Ready(server)) => lock(&server.docs).contains_key(path),
                Some(Slot::Unavailable(_)) => true,
                _ => false,
            };
            if open {
                continue;
            }
            if let Some(text) = read_text(workspace, path) {
                out.0.insert(path.clone(), text);
            }
        }
        out
    }

    /// Start `language`'s server, or the running one; `Err` with why it is
    /// unavailable.
    async fn server(
        &self,
        session: &Arc<LocalSession>,
        language: Language,
    ) -> Result<Arc<Server>, String> {
        let _pass = self.inner.starting.lock().await;
        let restarts = {
            let mut slots = lock(&self.inner.slots);
            match slots.get(&language) {
                Some(Slot::Ready(server)) if !server.process.exited() => {
                    return Ok(Arc::clone(server));
                }
                Some(Slot::Ready(server)) => {
                    let restarts = server.restarts + 1;
                    slots.insert(language, Slot::Crashed(restarts));
                    drop(slots);
                    self.inner.emit(
                        language,
                        LspState::Crashed,
                        Some("the language server exited".to_owned()),
                    );
                    restarts
                }
                Some(Slot::Crashed(count)) => *count,
                Some(Slot::Unavailable(why)) => return Err(why.clone()),
                None => 0,
            }
        };
        if restarts > MAX_RESTARTS {
            return Err(self.unavailable(
                language,
                format!(
                    "the {} language server keeps crashing; diagnostics off for {}",
                    language.server().0,
                    language.label()
                ),
            ));
        }
        let policy = session.rules().policy();
        if !(policy.allowed && policy.shell) {
            return Err(self.unavailable(
                language,
                "commands are off by policy, so language servers do not run; diagnostics off"
                    .to_owned(),
            ));
        }
        let Some((program, args)) = self.program(language) else {
            return Err(self.unavailable(
                language,
                format!(
                    "{} not found on PATH; diagnostics off for {}",
                    language.server().0,
                    language.label()
                ),
            ));
        };
        self.inner.emit(language, LspState::Starting, None);
        let launch = match launch::Launch::plan(session, language, &program, &args) {
            Ok(launch) => launch,
            Err(why) => return Err(self.unavailable(language, why)),
        };
        self.make_room(language);
        let process = match launch.spawn().await {
            Ok(process) => process,
            Err(why) => {
                return Err(self.unavailable(language, format!("{why}; diagnostics off")));
            }
        };
        let server = Arc::new(Server {
            id: self.inner.next_id.fetch_add(1, Ordering::SeqCst),
            restarts,
            process,
            docs: Mutex::new(HashMap::new()),
            last_used: Mutex::new(Instant::now()),
            fresh: Mutex::new(true),
            sync: tokio::sync::Mutex::new(()),
        });
        lock(&self.inner.slots).insert(language, Slot::Ready(Arc::clone(&server)));
        self.inner.emit(language, LspState::Ready, None);
        self.reaper(language, server.id);
        Ok(server)
    }

    fn unavailable(&self, language: Language, why: String) -> String {
        lock(&self.inner.slots).insert(language, Slot::Unavailable(why.clone()));
        self.inner
            .emit(language, LspState::Unavailable, Some(why.clone()));
        why
    }

    /// With [`MAX_SERVERS`] running, stop the one used least recently
    /// (another than `language`'s).
    fn make_room(&self, language: Language) {
        let evicted = {
            let mut slots = lock(&self.inner.slots);
            let running: Vec<(Language, Instant)> = slots
                .iter()
                .filter_map(|(other, slot)| match slot {
                    Slot::Ready(server) if *other != language && !server.process.exited() => {
                        Some((*other, *lock(&server.last_used)))
                    }
                    _ => None,
                })
                .collect();
            if running.len() < MAX_SERVERS {
                return;
            }
            let Some((oldest, _)) = running.iter().min_by_key(|(_, used)| *used) else {
                return;
            };
            slots.remove(oldest);
            *oldest
        };
        self.inner.emit(
            evicted,
            LspState::Stopped,
            Some(format!(
                "at most {MAX_SERVERS} language servers run per workspace"
            )),
        );
    }

    fn reaper(&self, language: Language, id: u64) {
        let weak: Weak<Inner> = Arc::downgrade(&self.inner);
        let idle = lock(&self.inner.settings).idle;
        let every = (idle / 4).clamp(Duration::from_millis(10), Duration::from_secs(30));
        tokio::spawn(async move {
            loop {
                tokio::time::sleep(every).await;
                let Some(inner) = weak.upgrade() else { break };
                if inner.reap(language, id) {
                    break;
                }
            }
        });
    }

    /// After an edit: tell the servers, wait for their diagnostics, and
    /// return the `diagnostics` field for the result (`None`: nothing to
    /// say, e.g. no file of a served language changed).
    pub(crate) async fn after(
        &self,
        session: &Arc<LocalSession>,
        before: Before,
        changed: Vec<Changed>,
    ) -> Option<Value> {
        let settings = self.settings();
        if !settings.enabled {
            return None;
        }
        let workspace = session.workspace();
        let mut by_language: BTreeMap<Language, Vec<(Changed, &'static str)>> = BTreeMap::new();
        for change in changed {
            if let Some((language, id)) = Self::served(workspace, &change.path) {
                by_language.entry(language).or_default().push((change, id));
            }
        }
        if by_language.is_empty() {
            return None;
        }
        let mut report = Report::default();
        for (language, changes) in by_language {
            let server = match self.server(session, language).await {
                Ok(server) => server,
                Err(why) => {
                    if lock(&self.inner.reported).insert(language) {
                        report.unavailable.push(why);
                    }
                    continue;
                }
            };
            let first = server.take_first();
            let wait = settings.wait(language, first);
            let outcome = sync_edit(session, &server, &before, changes, wait).await;
            server.touch();
            {
                let mut synced = lock(&self.inner.synced);
                for file in &outcome.files {
                    synced.insert(file.path.clone());
                }
            }
            report.merge(outcome);
        }
        report.render()
    }

    /// The `diagnostics` tool.
    pub(crate) async fn tool(
        &self,
        session: &Arc<LocalSession>,
        args: &DiagnosticsArgs,
        max_bytes: usize,
    ) -> ToolResult<Value> {
        let settings = self.settings();
        if !settings.enabled {
            return Ok(unavailable_listing(
                "diagnostics are turned off for this workspace",
            ));
        }
        let workspace = session.workspace();
        let mut found: Vec<(WsPath, Diag)> = Vec::new();
        let mut status = "ok";
        if let Some(path) = &args.path {
            let path = workspace.resolve(path, Intent::Read)?;
            let (language, id) = language_of(&path)
                .ok_or_else(|| ToolError::invalid(format!("no language server serves `{path}`")))?;
            if !registry::detected(workspace, language) {
                return Ok(unavailable_listing(&format!(
                    "no {} at the workspace root, so {} has no language server",
                    language.markers().join(" or "),
                    language.label()
                )));
            }
            let server = match self.server(session, language).await {
                Ok(server) => server,
                Err(why) => return Ok(unavailable_listing(&why)),
            };
            let first = server.take_first();
            let wait = settings.wait(language, first);
            let got = current(session, &server, &path, id, wait).await;
            server.touch();
            match got {
                Some(diags) => found.extend(diags.into_iter().map(|diag| (path.clone(), diag))),
                None => status = "timeout",
            }
            lock(&self.inner.synced).insert(path);
        } else {
            let synced: Vec<WsPath> = lock(&self.inner.synced).iter().cloned().collect();
            let slots: Vec<Arc<Server>> = lock(&self.inner.slots)
                .values()
                .filter_map(|slot| match slot {
                    Slot::Ready(server) => Some(Arc::clone(server)),
                    _ => None,
                })
                .collect();
            for path in synced {
                if workspace.denied_by(&path, Intent::Read).is_some() {
                    continue;
                }
                for server in &slots {
                    let key = lock(&server.docs).get(&path).map(|doc| doc.key.clone());
                    if let Some(published) =
                        key.and_then(|key| server.process.connection.published(&key))
                    {
                        found.extend(published.diags.into_iter().map(|diag| (path.clone(), diag)));
                    }
                }
            }
        }
        found.retain(|(_, diag)| args.severity.keeps(diag.severity));
        found.sort_by(|a, b| {
            (a.1.severity, &a.0, a.1.line, a.1.col).cmp(&(b.1.severity, &b.0, b.1.line, b.1.col))
        });
        let total = found.len();
        let max = args.max.clamp(1, 200);
        let mut items = Vec::new();
        let mut bytes = 0;
        for (path, diag) in found.iter().take(max) {
            let item = entry(path, diag);
            bytes += item.to_string().len();
            if bytes > max_bytes {
                break;
            }
            items.push(item);
        }
        let truncated = items.len() < total;
        Ok(json!({
            "diagnostics": {
                "status": status,
                "items": items,
                "total": total,
                "truncated": truncated,
            }
        }))
    }
}

impl Drop for LanguageServers {
    fn drop(&mut self) {
        // The processes stop as their slots go (`launch::Process`).
        lock(&self.inner.slots).clear();
    }
}

fn unavailable_listing(message: &str) -> Value {
    json!({
        "diagnostics": {
            "status": "unavailable",
            "message": message,
            "items": [],
            "total": 0,
            "truncated": false,
        }
    })
}

/// A file's text through the confined workspace; `None` when it is
/// missing, too large, not UTF-8 or refused.
fn read_text(workspace: &Workspace, path: &WsPath) -> Option<String> {
    if workspace.denied_by(path, Intent::Read).is_some() {
        return None;
    }
    let file = workspace.read(path, MAX_DOCUMENT_BYTES).ok()?;
    String::from_utf8(file.bytes).ok()
}

async fn read_blocking(session: &Arc<LocalSession>, path: &WsPath) -> Option<String> {
    let (session, path) = (Arc::clone(session), path.clone());
    tokio::task::spawn_blocking(move || read_text(session.workspace(), &path))
        .await
        .ok()
        .flatten()
}

/// What `diagnostics` takes.
#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DiagnosticsArgs {
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub severity: SeverityFilter,
    #[serde(default = "default_max")]
    pub max: usize,
}

const fn default_max() -> usize {
    50
}

/// Which diagnostics `diagnostics` lists.
#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub enum SeverityFilter {
    #[default]
    Error,
    /// Errors and warnings.
    Warning,
    All,
}

impl SeverityFilter {
    fn keeps(self, severity: Severity) -> bool {
        match self {
            Self::Error => severity == Severity::Error,
            Self::Warning => matches!(severity, Severity::Error | Severity::Warning),
            Self::All => true,
        }
    }
}

fn entry(path: &WsPath, diag: &Diag) -> Value {
    let mut item = json!({
        "path": path.display_string(),
        "line": diag.line,
        "col": diag.col,
        "severity": diag.severity,
        "message": diag.message,
    });
    if let Some(code) = &diag.code {
        item["code"] = Value::String(code.clone());
    }
    item
}

/// One file's outcome of a sync.
struct FileOutcome {
    path: WsPath,
    /// `None`: no answer in time.
    after: Option<Vec<Diag>>,
    before: Vec<Diag>,
}

#[derive(Default)]
struct SyncOutcome {
    files: Vec<FileOutcome>,
}

/// The `diagnostics` field being built.
#[derive(Default)]
struct Report {
    files: Vec<FileOutcome>,
    unavailable: Vec<String>,
}

impl Report {
    fn merge(&mut self, outcome: SyncOutcome) {
        self.files.extend(outcome.files);
    }

    fn render(self) -> Option<Value> {
        if self.files.is_empty() && self.unavailable.is_empty() {
            return None;
        }
        let mut new_errors = Vec::new();
        let mut new_warnings = Vec::new();
        let mut total_errors = 0;
        let mut timed_out = false;
        for file in &self.files {
            let Some(after) = &file.after else {
                timed_out = true;
                continue;
            };
            total_errors += after
                .iter()
                .filter(|diag| diag.severity == Severity::Error)
                .count();
            for diag in new_since(&file.before, after) {
                match diag.severity {
                    Severity::Error => new_errors.push((&file.path, diag)),
                    Severity::Warning => new_warnings.push((&file.path, diag)),
                    Severity::Information | Severity::Hint => {}
                }
            }
        }
        if new_warnings.len() <= MAX_NEW_WARNINGS {
            new_errors.extend(new_warnings);
        }
        let status = if self.files.is_empty() {
            "unavailable"
        } else if timed_out {
            "timeout"
        } else {
            "ok"
        };
        let total = new_errors.len();
        let mut items = Vec::new();
        let mut bytes = 0;
        for (path, diag) in new_errors.into_iter().take(MAX_REPORTED) {
            let item = entry(path, diag);
            bytes += item.to_string().len();
            if bytes > MAX_REPORT_BYTES {
                break;
            }
            items.push(item);
        }
        let mut out = json!({
            "status": status,
            "new_errors": items,
            "total_errors_in_file": total_errors,
        });
        if items_len(&out) < total {
            out["truncated"] = Value::Bool(true);
        }
        if !self.unavailable.is_empty() {
            out["message"] = Value::String(self.unavailable.join("; "));
        }
        Some(out)
    }
}

fn items_len(out: &Value) -> usize {
    out["new_errors"].as_array().map_or(0, Vec::len)
}

/// The diagnostics of `after` that `before` did not have, compared by
/// severity, code and message (an edit moves lines), as many times as
/// they are new.
fn new_since<'a>(before: &[Diag], after: &'a [Diag]) -> Vec<&'a Diag> {
    let mut seen: HashMap<(Severity, Option<&str>, &str), usize> = HashMap::new();
    for diag in before {
        *seen.entry(diag.key()).or_default() += 1;
    }
    after
        .iter()
        .filter(|diag| match seen.get_mut(&diag.key()) {
            Some(count) if *count > 0 => {
                *count -= 1;
                false
            }
            _ => true,
        })
        .collect()
}

fn absolute_key(workspace: &Workspace, path: &WsPath) -> String {
    workspace.absolute(path).display().to_string()
}

/// Sync one server's changed files and wait for their diagnostics.
async fn sync_edit(
    session: &Arc<LocalSession>,
    server: &Arc<Server>,
    before: &Before,
    changes: Vec<(Changed, &'static str)>,
    wait: Duration,
) -> SyncOutcome {
    let _one = server.sync.lock().await;
    let connection = &server.process.connection;
    let workspace = session.workspace();
    // 1. Open what the server has not seen, with its old text where it
    //    had one, and wait (one deadline for all) for those diagnostics.
    let mut baselines: Vec<(WsPath, String, u64)> = Vec::new();
    let mut targets: Vec<(WsPath, String)> = Vec::new();
    for (change, language_id) in changes {
        let path = change.path;
        let key = absolute_key(workspace, &path);
        if change.deleted {
            let doc = lock(&server.docs).remove(&path);
            if let Some(doc) = doc {
                let _ = connection
                    .notify(
                        "textDocument/didClose",
                        json!({ "textDocument": { "uri": doc.uri } }),
                    )
                    .await;
            }
            continue;
        }
        let open = lock(&server.docs).contains_key(&path);
        if !open && let Some(old) = before.0.get(&path) {
            let seq = connection.seq();
            if open_doc(server, &path, &key, language_id, old).await {
                baselines.push((path.clone(), key.clone(), seq));
            }
        }
        targets.push((path, key));
    }
    let deadline = tokio::time::Instant::now() + wait;
    let mut known: HashMap<WsPath, Vec<Diag>> = HashMap::new();
    for (path, key, seq) in baselines {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        if let Some(published) = connection.wait_publish(&key, seq, Some(1), left).await {
            known.insert(path, published.diags);
        }
    }
    // 2. Send the new text and wait for its diagnostics.
    let mut pending: Vec<(WsPath, String, u64, i32, Vec<Diag>)> = Vec::new();
    for (path, key) in targets {
        let Some(text) = read_blocking(session, &path).await else {
            continue;
        };
        let language_id = language_of(&path).map_or("plaintext", |(_, id)| id);
        let base = known.remove(&path).unwrap_or_else(|| {
            connection
                .published(&key)
                .map(|published| published.diags)
                .unwrap_or_default()
        });
        let seq = connection.seq();
        let version = if lock(&server.docs).contains_key(&path) {
            match change_doc(server, &path, &text).await {
                Some(version) => version,
                None => continue,
            }
        } else if open_doc(server, &path, &key, language_id, &text).await {
            1
        } else {
            continue;
        };
        pending.push((path, key, seq, version, base));
    }
    let deadline = tokio::time::Instant::now() + wait;
    let mut outcome = SyncOutcome::default();
    for (path, key, seq, version, before) in pending {
        let left = deadline.saturating_duration_since(tokio::time::Instant::now());
        let mut after = connection
            .wait_publish(&key, seq, Some(version), left)
            .await;
        if let Some(got) = &after {
            let quiet = SETTLE.min(deadline.saturating_duration_since(tokio::time::Instant::now()));
            if let Some(later) = connection.settle(&key, got.seq, quiet).await {
                after = Some(later);
            }
        }
        outcome.files.push(FileOutcome {
            path,
            after: after.map(|published| published.diags),
            before,
        });
    }
    outcome
}

async fn open_doc(
    server: &Server,
    path: &WsPath,
    key: &str,
    language_id: &'static str,
    text: &str,
) -> bool {
    let uri = client::path_uri(key);
    let sent = server
        .process
        .connection
        .notify(
            "textDocument/didOpen",
            json!({
                "textDocument": {
                    "uri": uri,
                    "languageId": language_id,
                    "version": 1,
                    "text": text,
                }
            }),
        )
        .await
        .is_ok();
    if sent {
        lock(&server.docs).insert(
            path.clone(),
            Doc {
                key: key.to_owned(),
                uri,
                version: 1,
            },
        );
    }
    sent
}

/// Send `text` as the open document's new version (`didChange`, then
/// `didSave`); its version.
async fn change_doc(server: &Server, path: &WsPath, text: &str) -> Option<i32> {
    let (uri, version) = {
        let mut docs = lock(&server.docs);
        let doc = docs.get_mut(path)?;
        doc.version = doc.version.saturating_add(1);
        (doc.uri.clone(), doc.version)
    };
    let connection = &server.process.connection;
    connection
        .notify(
            "textDocument/didChange",
            json!({
                "textDocument": { "uri": uri, "version": version },
                "contentChanges": [{ "text": text }],
            }),
        )
        .await
        .ok()?;
    let _ = connection
        .notify(
            "textDocument/didSave",
            json!({ "textDocument": { "uri": uri } }),
        )
        .await;
    Some(version)
}

/// The current diagnostics of `path` for the `diagnostics` tool: what the
/// server published for the open document, else the document opened now
/// and its first answer; `None` when none came within `wait`.
async fn current(
    session: &Arc<LocalSession>,
    server: &Arc<Server>,
    path: &WsPath,
    language_id: &'static str,
    wait: Duration,
) -> Option<Vec<Diag>> {
    let _one = server.sync.lock().await;
    let connection = &server.process.connection;
    let key = absolute_key(session.workspace(), path);
    let open = lock(&server.docs).get(path).map(|doc| doc.version);
    if let Some(version) = open {
        if let Some(published) = connection.published(&key) {
            return Some(published.diags);
        }
        return connection
            .wait_publish(&key, 0, Some(version), wait)
            .await
            .map(|published| published.diags);
    }
    let text = read_blocking(session, path).await?;
    let seq = connection.seq();
    if !open_doc(server, path, &key, language_id, &text).await {
        return None;
    }
    connection
        .wait_publish(&key, seq, Some(1), wait)
        .await
        .map(|published| published.diags)
}
