//! A workspace session's local MCP servers (#1230): started lazily (by the
//! first turn that offers their tools), kept for the session, stopped with
//! it, and stopped when their configuration changes.
//!
//! Every server is a stdio child built by the same [`sandbox::prepare`]
//! call `run_command` uses, with the session's request (its `DenyList`,
//! `path_deny`, `.git` read-only): mode `workspace-write` capped by the
//! policy's `max_sandbox_mode` (`read-only` when every tool it may offer is
//! marked read-only), the network from its configuration capped by the
//! policy (loopback included: off means off). Its environment is cleared
//! and holds the shell's scrubbed passthrough ([`crate::shell`]: `PATH`,
//! `HOME`, `USER`, `LANG`, …, with `TMPDIR` its own private directory)
//! plus exactly the variables its configuration lists; nothing else of the
//! host's environment reaches it. It runs in its own process group, which
//! is what is stopped: `SIGTERM`, then `SIGKILL` after
//! [`crate::process::TERM_GRACE`], as background processes are.
//!
//! The start is a [`ToolKind::McpStart`] the rules decide (see
//! [`crate::approvals`]): one whose sandbox cannot hide what the session
//! denies is asked at every start, one with the network once per session
//! of the app. `initialize` and `tools/list` each have [`START_TIMEOUT`]; a
//! server that fails is reported in its [`ServerStatus`] and offers no
//! tools, never failing the turn. Its stderr is kept in a ring of
//! [`STDERR_RING`] bytes for the settings pane: never logged, never sent
//! anywhere else.

use std::collections::{BTreeMap, HashSet};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use rmcp::ServiceExt as _;
use rmcp::model::{CallToolRequestParams, Tool as McpToolInfo};
use rmcp::service::{RoleClient, RunningService};
use serde::Serialize;
use serde_json::Value;
use tokio::io::AsyncReadExt as _;

use super::config::{Scope, ServerConfig, ServerCwd};
use crate::approvals::{McpTarget, ToolCall, ToolKind};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;
use crate::process::TERM_GRACE;
use crate::sandbox::{self, Enforcement};
use crate::session::LocalSession;
use crate::shell;

/// How long `initialize`, and then `tools/list`, may take.
pub const START_TIMEOUT: Duration = Duration::from_secs(10);

/// How long one `tools/call` may take.
pub const CALL_TIMEOUT: Duration = Duration::from_secs(120);

/// The stderr bytes kept per server.
pub const STDERR_RING: usize = 64 * 1024;

/// How much of the ring a status carries.
const STDERR_TAIL: usize = 8 * 1024;

/// Where a server is.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ServerState {
    /// Configured, not started yet (no turn has offered it).
    Idle,
    Starting,
    Ready,
    Failed,
    /// Turned off in its configuration, or stopped.
    Stopped,
}

/// One server, as `mcp_status` shows it.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct ServerStatus {
    pub name: String,
    pub scope: Scope,
    pub state: ServerState,
    /// The tools it lists (its own names), when ready.
    pub tools: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    /// The end of its stderr (at most 8 KiB of the 64 KiB ring).
    pub stderr_tail: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enforcement: Option<&'static str>,
    pub network: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sandbox: Option<&'static str>,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// The last [`STDERR_RING`] bytes a server wrote to stderr.
#[derive(Default)]
struct Ring(Vec<u8>);

impl Ring {
    fn push(&mut self, bytes: &[u8]) {
        self.0.extend_from_slice(bytes);
        if self.0.len() > STDERR_RING {
            let cut = self.0.len() - STDERR_RING;
            self.0.drain(..cut);
        }
    }

    fn tail(&self) -> String {
        let from = self.0.len().saturating_sub(STDERR_TAIL);
        String::from_utf8_lossy(&self.0[from..]).into_owned()
    }
}

/// A started server.
pub struct Running {
    service: RunningService<RoleClient, ()>,
    tools: Vec<McpToolInfo>,
    group: Option<rustix::process::Pid>,
    exited: Arc<AtomicBool>,
    exit: Arc<Mutex<Option<String>>>,
    changed: Arc<tokio::sync::Notify>,
    stderr: Arc<Mutex<Ring>>,
    enforcement: Enforcement,
    network: bool,
    mode: SandboxMode,
}

impl Running {
    /// The tools it listed when it started.
    #[must_use]
    pub fn tools(&self) -> &[McpToolInfo] {
        &self.tools
    }

    /// Whether its process has ended.
    #[must_use]
    pub fn exited(&self) -> bool {
        self.exited.load(Ordering::SeqCst)
    }

    /// Call one of its tools, at most [`CALL_TIMEOUT`]; the result as JSON
    /// (`content`, `structuredContent`, `isError`).
    ///
    /// # Errors
    ///
    /// The server stopped, timed out or answered with an error.
    pub async fn call(&self, tool: &str, arguments: Value) -> ToolResult<Value> {
        if self.exited() {
            return Err(ToolError::new(
                ErrorCode::Unsupported,
                "the MCP server has stopped; restart it in Settings",
            ));
        }
        let arguments = match arguments {
            Value::Object(map) => map,
            Value::Null => serde_json::Map::new(),
            _ => return Err(ToolError::invalid("the arguments must be an object")),
        };
        let params = CallToolRequestParams::new(tool.to_owned()).with_arguments(arguments);
        let result = tokio::time::timeout(CALL_TIMEOUT, self.service.call_tool(params))
            .await
            .map_err(|_| {
                ToolError::new(ErrorCode::Timeout, "the MCP server did not answer in time")
            })?
            .map_err(|error| {
                ToolError::new(
                    ErrorCode::Io,
                    format!("the MCP server failed the call: {error}"),
                )
            })?;
        serde_json::to_value(result)
            .map_err(|_| ToolError::new(ErrorCode::Io, "the MCP server's answer is not JSON"))
    }

    fn signal(&self, signal: rustix::process::Signal) {
        if let Some(group) = self.group
            && !self.exited()
        {
            let _ = rustix::process::kill_process_group(group, signal);
        }
    }

    /// `SIGTERM` its group now and `SIGKILL` it after [`TERM_GRACE`], off
    /// this thread.
    fn stop_soon(self: &Arc<Self>) {
        if self.exited() {
            return;
        }
        self.signal(rustix::process::Signal::TERM);
        let this = Arc::clone(self);
        let _ = std::thread::Builder::new()
            .name("elitea-mcp-stop".to_owned())
            .spawn(move || {
                let deadline = Instant::now() + TERM_GRACE;
                while !this.exited() && Instant::now() < deadline {
                    std::thread::sleep(Duration::from_millis(20));
                }
                this.signal(rustix::process::Signal::KILL);
            });
    }

    async fn stop(&self) {
        self.signal(rustix::process::Signal::TERM);
        if !self.wait_exit(TERM_GRACE).await {
            self.signal(rustix::process::Signal::KILL);
            let _ = self.wait_exit(TERM_GRACE).await;
        }
    }

    async fn wait_exit(&self, timeout: Duration) -> bool {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            let notified = self.changed.notified();
            if self.exited() {
                return true;
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return self.exited();
            }
        }
    }
}

enum State {
    Idle,
    Starting,
    Ready(Arc<Running>),
    Failed { error: String, stderr: String },
    Stopped,
}

struct Entry {
    scope: Scope,
    config: ServerConfig,
    state: State,
}

impl Entry {
    fn status(&self, name: &str) -> ServerStatus {
        let mut status = ServerStatus {
            name: name.to_owned(),
            scope: self.scope,
            state: ServerState::Idle,
            tools: Vec::new(),
            error: None,
            stderr_tail: String::new(),
            enforcement: None,
            network: false,
            sandbox: None,
        };
        match &self.state {
            State::Idle if !self.config.enabled => status.state = ServerState::Stopped,
            State::Idle => {}
            State::Starting => status.state = ServerState::Starting,
            State::Stopped => status.state = ServerState::Stopped,
            State::Failed { error, stderr } => {
                status.state = ServerState::Failed;
                status.error = Some(error.clone());
                status.stderr_tail.clone_from(stderr);
            }
            State::Ready(running) => {
                status.stderr_tail = lock(&running.stderr).tail();
                status.enforcement = Some(running.enforcement.as_str());
                status.network = running.network;
                status.sandbox = Some(running.mode.as_str());
                if running.exited() {
                    status.state = ServerState::Failed;
                    status.error = Some(
                        lock(&running.exit)
                            .clone()
                            .unwrap_or_else(|| "the server exited".to_owned()),
                    );
                } else {
                    status.state = ServerState::Ready;
                    status.tools = running
                        .tools
                        .iter()
                        .map(|tool| tool.name.to_string())
                        .collect();
                }
            }
        }
        status
    }
}

/// A ready server's tools, for the provider.
pub struct ReadyServer {
    pub name: String,
    pub config: ServerConfig,
    pub running: Arc<Running>,
}

/// One session's local MCP servers. Dropping it stops every one.
#[derive(Default)]
pub struct McpServers {
    entries: Mutex<BTreeMap<String, Entry>>,
    /// Servers (name and configuration) whose network start the person
    /// approved in this session of the app: not asked again until the
    /// configuration changes.
    network_approved: Mutex<HashSet<String>>,
    /// One start pass at a time.
    starting: tokio::sync::Mutex<()>,
}

fn fingerprint(name: &str, config: &ServerConfig) -> String {
    format!(
        "{name}\0{}",
        serde_json::to_string(config).unwrap_or_default()
    )
}

impl McpServers {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Take the servers the person configures now (both files, merged): a
    /// server whose entry changed or went is stopped; a new or changed one
    /// starts with the next turn that offers MCP tools.
    pub fn configure(&self, servers: BTreeMap<String, (Scope, ServerConfig)>) {
        let mut entries = lock(&self.entries);
        let mut stopping = Vec::new();
        entries.retain(|name, entry| {
            let keep = servers
                .get(name)
                .is_some_and(|(scope, config)| *scope == entry.scope && *config == entry.config);
            if !keep && let State::Ready(running) = &entry.state {
                stopping.push(Arc::clone(running));
            }
            keep
        });
        for (name, (scope, config)) in servers {
            entries.entry(name).or_insert(Entry {
                scope,
                config,
                state: State::Idle,
            });
        }
        drop(entries);
        for running in stopping {
            running.stop_soon();
        }
    }

    /// Every configured server's status, by name.
    #[must_use]
    pub fn status(&self) -> Vec<ServerStatus> {
        lock(&self.entries)
            .iter()
            .map(|(name, entry)| entry.status(name))
            .collect()
    }

    /// The servers that are ready now, by name.
    #[must_use]
    pub fn ready(&self) -> Vec<ReadyServer> {
        lock(&self.entries)
            .iter()
            .filter_map(|(name, entry)| match &entry.state {
                State::Ready(running) if !running.exited() => Some(ReadyServer {
                    name: name.clone(),
                    config: entry.config.clone(),
                    running: Arc::clone(running),
                }),
                _ => None,
            })
            .collect()
    }

    /// Start every enabled server that has not started (a failed one waits
    /// for [`Self::restart`] or a change of its configuration); `subject`
    /// names the questions asked. The statuses after the pass.
    pub async fn start_all(&self, session: &Arc<LocalSession>, subject: &str) -> Vec<ServerStatus> {
        let _pass = self.starting.lock().await;
        let pending: Vec<(String, ServerConfig)> = lock(&self.entries)
            .iter_mut()
            .filter(|(_, entry)| entry.config.enabled && matches!(entry.state, State::Idle))
            .map(|(name, entry)| {
                entry.state = State::Starting;
                (name.clone(), entry.config.clone())
            })
            .collect();
        for (name, config) in pending {
            let started = self.start_one(session, subject, &name, &config).await;
            let mut entries = lock(&self.entries);
            match entries.get_mut(&name) {
                // Reconfigured meanwhile: the new entry starts next time.
                Some(entry) if entry.config == config && matches!(entry.state, State::Starting) => {
                    entry.state = match started {
                        Ok(running) => State::Ready(running),
                        Err((error, stderr)) => State::Failed { error, stderr },
                    };
                }
                _ => {
                    if let Ok(running) = started {
                        running.stop_soon();
                    }
                }
            }
        }
        self.status()
    }

    /// Stop `name` and start it again (the person's Restart).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::NotFound`] for a server that is not configured.
    pub async fn restart(
        &self,
        session: &Arc<LocalSession>,
        name: &str,
    ) -> ToolResult<ServerStatus> {
        let previous = {
            let mut entries = lock(&self.entries);
            let entry = entries.get_mut(name).ok_or_else(|| {
                ToolError::new(ErrorCode::NotFound, format!("no MCP server `{name}`"))
            })?;
            std::mem::replace(&mut entry.state, State::Idle)
        };
        if let State::Ready(running) = previous {
            running.stop().await;
        }
        let statuses = self
            .start_all(session, &format!("mcp_restart:{name}"))
            .await;
        statuses
            .into_iter()
            .find(|status| status.name == name)
            .ok_or_else(|| ToolError::new(ErrorCode::NotFound, format!("no MCP server `{name}`")))
    }

    fn take_running(&self) -> Vec<Arc<Running>> {
        lock(&self.entries)
            .values_mut()
            .filter_map(|entry| {
                match std::mem::replace(&mut entry.state, State::Stopped) {
                    State::Ready(running) => Some(running),
                    State::Starting => {
                        // A pass in flight finds it changed and stops it.
                        entry.state = State::Starting;
                        None
                    }
                    State::Idle => {
                        entry.state = State::Idle;
                        None
                    }
                    other => {
                        entry.state = other;
                        None
                    }
                }
            })
            .collect()
    }

    /// Stop every server (the policy turned them off, the workspace is
    /// removed, sign-out); a later turn starts them again if it may.
    pub async fn stop_all(&self) {
        let running = self.take_running();
        for server in &running {
            server.signal(rustix::process::Signal::TERM);
        }
        for server in &running {
            if !server.wait_exit(TERM_GRACE).await {
                server.signal(rustix::process::Signal::KILL);
            }
        }
        self.reset_stopped();
    }

    /// The first half of [`Self::stop_all`] from synchronous code (the app
    /// quits): `SIGTERM` every running server now, and return them for
    /// [`Self::kill_left`] once the caller's grace is over.
    pub fn terminate_all(&self) -> Vec<Arc<Running>> {
        let running = self.take_running();
        for server in &running {
            server.signal(rustix::process::Signal::TERM);
        }
        running
    }

    /// The second half of [`Self::terminate_all`]: `SIGKILL` the servers
    /// that are still there.
    pub fn kill_left(&self, running: &[Arc<Running>]) {
        for server in running {
            server.signal(rustix::process::Signal::KILL);
        }
        self.reset_stopped();
    }

    /// Stopped servers start again with the next turn.
    fn reset_stopped(&self) {
        for entry in lock(&self.entries).values_mut() {
            if matches!(entry.state, State::Stopped) {
                entry.state = State::Idle;
            }
        }
    }

    async fn start_one(
        &self,
        session: &Arc<LocalSession>,
        subject: &str,
        name: &str,
        config: &ServerConfig,
    ) -> Result<Arc<Running>, (String, String)> {
        let fail = |error: ToolError| (error.message().to_owned(), String::new());
        let launch = Launch::plan(session, name, config).map_err(fail)?;
        let fingerprint = fingerprint(name, config);
        let approved_before = launch.network && lock(&self.network_approved).contains(&fingerprint);
        let call = ToolCall {
            command: Some(
                shlex::try_join(launch.argv.iter().map(String::as_str)).unwrap_or_default(),
            ),
            sandbox: Some(launch.mode),
            // Asked once per session of the app: a later start of the same
            // configuration is decided as if offline (the policy has already
            // capped it).
            network: launch.network && !approved_before,
            unconfined: !launch.prepared.hides_denied,
            mcp: Some(McpTarget {
                server: name.to_owned(),
                tool: None,
                read_only: false,
            }),
            ..ToolCall::new(ToolKind::McpStart)
        };
        session
            .authorize(
                &format!("{subject}:mcp_start:{name}"),
                call,
                &format!("start the MCP server {name}"),
            )
            .await
            .map_err(fail)?;
        if launch.network {
            lock(&self.network_approved).insert(fingerprint);
        }
        launch.spawn().await
    }
}

impl Drop for McpServers {
    fn drop(&mut self) {
        for running in self.take_running() {
            running.stop_soon();
        }
    }
}

/// The environment a server gets: `base` (the shell's scrubbed
/// passthrough, never the host's whole environment) with `TMPDIR` its own,
/// then the variables its configuration lists.
#[must_use]
pub fn server_environment(
    base: Vec<(String, String)>,
    temp: &Path,
    config: &ServerConfig,
) -> Vec<(String, String)> {
    let mut env: Vec<(String, String)> = base
        .into_iter()
        .filter(|(name, _)| name != "TMPDIR" && !config.env.contains_key(name))
        .collect();
    env.push(("TMPDIR".to_owned(), temp.display().to_string()));
    env.extend(
        config
            .env
            .iter()
            .map(|(name, value)| (name.clone(), value.clone())),
    );
    env
}

/// Everything decided before a server starts.
struct Launch {
    argv: Vec<String>,
    prepared: sandbox::Prepared,
    env: Vec<(String, String)>,
    cwd: PathBuf,
    mode: SandboxMode,
    network: bool,
}

impl Launch {
    fn plan(session: &LocalSession, name: &str, config: &ServerConfig) -> ToolResult<Self> {
        let policy = session.rules().policy();
        if !(policy.allowed && policy.local_mcp) {
            return Err(ToolError::new(
                ErrorCode::Denied,
                "local MCP servers are disabled by policy",
            ));
        }
        // Resolved once: what runs is this file, whatever the name points
        // at later.
        let command = std::fs::canonicalize(&config.command).map_err(|_| {
            ToolError::new(
                ErrorCode::NotFound,
                format!("`{}` does not exist", config.command),
            )
        })?;
        let executable = {
            use std::os::unix::fs::PermissionsExt as _;
            command
                .metadata()
                .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
        };
        if !executable {
            return Err(ToolError::invalid(format!(
                "`{}` is not an executable file",
                config.command
            )));
        }
        let workspace = session.workspace();
        if command.starts_with(workspace.root()) {
            return Err(ToolError::new(
                ErrorCode::Denied,
                "a server's program cannot be inside the workspace",
            ));
        }
        if session.deny_list().covers(&command) {
            return Err(ToolError::new(
                ErrorCode::Denied,
                "a server's program cannot be in a denied folder",
            ));
        }
        let mode = if config.all_read_only() {
            SandboxMode::ReadOnly
        } else {
            SandboxMode::WorkspaceWrite.min(policy.max_sandbox_mode)
        };
        let network = config.network && policy.network;
        let shell = session.shell_config();
        let temp = shell.temp_dir.join("mcp").join(name);
        {
            use std::os::unix::fs::DirBuilderExt as _;
            std::fs::DirBuilder::new()
                .recursive(true)
                .mode(0o700)
                .create(&temp)
                .map_err(|error| {
                    ToolError::io("cannot create the server's temporary folder", &error)
                })?;
        }
        let temp = std::fs::canonicalize(&temp).map_err(|error| {
            ToolError::io("cannot resolve the server's temporary folder", &error)
        })?;
        let request = session.sandbox_request(mode, network)?;
        let mut argv = vec![command.display().to_string()];
        argv.extend(config.args.iter().cloned());
        let prepared = sandbox::prepare(&request, &argv, &shell.sandbox)?;
        let cwd = match config.cwd {
            ServerCwd::Workspace => workspace.root().to_path_buf(),
            ServerCwd::None => temp.clone(),
        };
        let env = server_environment(shell::process_environment(shell, false), &temp, config);
        Ok(Self {
            argv,
            prepared,
            env,
            cwd,
            mode,
            network,
        })
    }

    async fn spawn(self) -> Result<Arc<Running>, (String, String)> {
        let mut command = tokio::process::Command::new(&self.prepared.program);
        command
            .args(&self.prepared.args)
            .env_clear()
            .envs(self.env.iter().map(|(name, value)| (name, value)))
            .current_dir(&self.cwd)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .process_group(0)
            .kill_on_drop(true);
        let mut child = command.spawn().map_err(|error| {
            (
                format!("the server could not start: {}", error.kind()),
                String::new(),
            )
        })?;
        let group = child
            .id()
            .and_then(|pid| i32::try_from(pid).ok())
            .and_then(rustix::process::Pid::from_raw);
        let (Some(stdin), Some(stdout), Some(mut stderr)) =
            (child.stdin.take(), child.stdout.take(), child.stderr.take())
        else {
            return Err(("the server has no pipes".to_owned(), String::new()));
        };
        let ring = Arc::new(Mutex::new(Ring::default()));
        {
            let ring = Arc::clone(&ring);
            tokio::spawn(async move {
                let mut buffer = [0u8; 4096];
                while let Ok(read) = stderr.read(&mut buffer).await {
                    if read == 0 {
                        break;
                    }
                    lock(&ring).push(&buffer[..read]);
                }
            });
        }
        let exited = Arc::new(AtomicBool::new(false));
        let exit = Arc::new(Mutex::new(None));
        let changed = Arc::new(tokio::sync::Notify::new());
        {
            let (exited, exit, changed) =
                (Arc::clone(&exited), Arc::clone(&exit), Arc::clone(&changed));
            tokio::spawn(async move {
                let status = child.wait().await;
                *lock(&exit) = Some(match status {
                    Ok(status) => match status.code() {
                        Some(code) => format!("the server exited with code {code}"),
                        None => "the server was stopped by a signal".to_owned(),
                    },
                    Err(_) => "the server exited".to_owned(),
                });
                exited.store(true, Ordering::SeqCst);
                changed.notify_waiters();
            });
        }
        let kill = |why: String| {
            if let Some(group) = group {
                let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
            }
            (why, lock(&ring).tail())
        };
        let service = match tokio::time::timeout(START_TIMEOUT, ().serve((stdout, stdin))).await {
            Ok(Ok(service)) => service,
            Ok(Err(error)) => return Err(kill(format!("the server did not initialize: {error}"))),
            Err(_) => return Err(kill("the server did not initialize within 10 s".to_owned())),
        };
        let tools = match tokio::time::timeout(START_TIMEOUT, service.peer().list_all_tools()).await
        {
            Ok(Ok(tools)) => tools,
            Ok(Err(error)) => {
                return Err(kill(format!("the server did not list its tools: {error}")));
            }
            Err(_) => {
                return Err(kill(
                    "the server did not list its tools within 10 s".to_owned(),
                ));
            }
        };
        Ok(Arc::new(Running {
            service,
            tools,
            group,
            exited,
            exit,
            changed,
            stderr: ring,
            enforcement: self.prepared.enforcement,
            network: self.network,
            mode: self.mode,
        }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn env_values_are_never_inherited_from_parent() {
        // The base is the shell's scrub, never `std::env::vars()`: whatever
        // the host holds (cargo sets CARGO_MANIFEST_DIR for this test) is
        // not in it unless the configuration names it.
        let base = vec![
            ("PATH".to_owned(), "/usr/bin:/bin".to_owned()),
            ("TMPDIR".to_owned(), "/tmp/session".to_owned()),
        ];
        let mut config = ServerConfig::new("/usr/bin/true");
        config
            .env
            .insert("GITHUB_TOKEN_FILE".to_owned(), "/x/token".to_owned());
        config.env.insert("PATH".to_owned(), "/opt/bin".to_owned());
        let env = server_environment(base, Path::new("/tmp/server"), &config);
        let get = |name: &str| {
            env.iter()
                .find(|(key, _)| key == name)
                .map(|(_, value)| value.as_str())
        };
        assert!(std::env::var_os("CARGO_MANIFEST_DIR").is_some());
        assert_eq!(get("CARGO_MANIFEST_DIR"), None);
        assert_eq!(get("TMPDIR"), Some("/tmp/server"));
        assert_eq!(get("GITHUB_TOKEN_FILE"), Some("/x/token"));
        assert_eq!(get("PATH"), Some("/opt/bin"), "the configuration wins");
        assert_eq!(env.iter().filter(|(key, _)| key == "PATH").count(), 1);
    }

    #[test]
    fn the_stderr_ring_keeps_its_end() {
        let mut ring = Ring::default();
        ring.push(&vec![b'a'; STDERR_RING]);
        ring.push(b"tail");
        assert_eq!(ring.0.len(), STDERR_RING);
        assert!(ring.tail().ends_with("tail"));
        assert!(ring.tail().len() <= STDERR_TAIL);
    }
}
