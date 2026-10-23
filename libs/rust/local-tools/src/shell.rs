//! Running one command: working directory inside the workspace, a scrubbed
//! environment, a timeout that kills the whole process group, output caps,
//! and the OS sandbox around it. Commands that outlive the call (in the
//! background, on a pseudo-terminal) are [`crate::process`]'s; they start
//! from the same request and environment ([`process_environment`]).

use std::fmt::Write as _;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use serde::Serialize;
use tokio::io::{AsyncRead, AsyncReadExt};

use crate::command::{CommandShape, analyse};
use crate::deny::DenyList;
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;
use crate::sandbox::{Enforcement, Prepared, SandboxConfig, SandboxRequest, prepare};
use crate::workspace::{EntryKind, Workspace, WsPath};

/// Variables passed through from the host's environment; everything else
/// is dropped (tokens, cloud credentials, the host's own settings).
const PASSTHROUGH: &[&str] = &[
    "PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
];

/// Variables never passed, even when a host lists them.
const SECRET_MARKERS: &[&str] = &[
    "TOKEN",
    "SECRET",
    "PASSWORD",
    "PASSWD",
    "CREDENTIAL",
    "API_KEY",
    "PRIVATE_KEY",
    "AUTH",
];

/// How long the readers may drain after the process group is killed (a
/// grandchild that left the group can hold a pipe open).
const DRAIN_AFTER_KILL: Duration = Duration::from_secs(2);

/// Bytes [`ShellConfig::new`] keeps per stream.
pub const DEFAULT_OUTPUT_CAP: usize = 4 * 1024 * 1024;

/// The host's command settings.
#[derive(Clone, Debug)]
#[allow(clippy::struct_excessive_bools)] // independent host switches
pub struct ShellConfig {
    pub default_timeout: Duration,
    pub max_timeout: Duration,
    /// The longest `run_tests` may run (a suite is slower than a
    /// command); its default is [`Self::max_timeout`].
    pub max_test_timeout: Duration,
    /// Bytes kept per stream (half from the start, half from the end):
    /// what a session spills when the output is over its token budget
    /// ([`crate::budget`]), which is what the model sees.
    pub output_cap: usize,
    /// More variable names to pass through (never secret-looking ones).
    pub env_passthrough: Vec<String>,
    /// The session's temporary directory (`TMPDIR`; writable in
    /// workspace-write).
    pub temp_dir: PathBuf,
    /// The build tools' cache root, kept across sessions (`go`, `gradle`
    /// and `m2` inside: Go's build cache, Gradle's home, Maven's local
    /// repository; writable in workspace-write). `None`: `build-cache`
    /// under [`Self::temp_dir`]. [`crate::session::LocalSession::open`]
    /// sets `<data_dir>/build-cache` when the host left it `None`.
    pub cache_dir: Option<PathBuf>,
    pub sandbox: SandboxConfig,
    /// More files or directories no tool may read: the host app's own
    /// config, data, log and cache directories as the host resolved them
    /// (the authority for those), and the session's data directory (the
    /// session adds it). Both spellings are denied (see
    /// [`crate::deny`]).
    pub deny_read: Vec<PathBuf>,
    /// Deny the credentials ([`crate::sandbox::credential_paths`]) and the
    /// app's identifier directories under the home directory (on by
    /// default).
    pub protect_credentials: bool,
    /// The home directory those are looked up under, also the commands'
    /// `HOME`. `None`: the host's `HOME`. For tests and hosts that run
    /// commands under another home.
    pub home: Option<PathBuf>,
    /// The host app's identifier (a bundle id such as `com.example.app`):
    /// its default directories under the home are denied too
    /// ([`crate::deny::app_paths`]). The host's resolved directories in
    /// [`Self::deny_read`] remain the authority.
    pub app_id: Option<String>,
    /// The session's deny list, once [`Self::freeze_deny_list`] built it.
    frozen: Option<Arc<DenyList>>,
    /// Let commands with the network on listen for connections (off by
    /// default).
    pub allow_listen: bool,
    /// Let commands reach the keychain's services (off by default).
    pub allow_keychain: bool,
    /// The global git config the host's git reads (`None`: the person's,
    /// from `HOME`); for hosts that keep it elsewhere and for tests.
    pub git_global_config: Option<PathBuf>,
    /// Offer background processes (`run_command`'s `background`, `tty`
    /// and `keep_alive`, and the `process_*` tools; [`crate::process`]).
    /// On by default; the cloud executor's runner turns it off (it keeps no
    /// process past a call).
    pub background_processes: bool,
    /// The longest a background process runs before it is stopped
    /// (`timed_out`); 4 hours by default.
    pub max_process_wall: Duration,
}

impl ShellConfig {
    /// Defaults: 2 minutes (at most 10; tests at most 30), 4 MiB kept per
    /// stream.
    #[must_use]
    pub fn new(temp_dir: PathBuf) -> Self {
        Self {
            default_timeout: Duration::from_mins(2),
            max_timeout: Duration::from_mins(10),
            max_test_timeout: Duration::from_mins(30),
            output_cap: DEFAULT_OUTPUT_CAP,
            env_passthrough: Vec::new(),
            temp_dir,
            cache_dir: None,
            sandbox: SandboxConfig::default(),
            deny_read: Vec::new(),
            protect_credentials: true,
            home: None,
            app_id: None,
            frozen: None,
            allow_listen: false,
            allow_keychain: false,
            git_global_config: None,
            background_processes: true,
            max_process_wall: Duration::from_hours(4),
        }
    }

    /// The home directory credentials are looked up under: [`Self::home`],
    /// else a non-empty `HOME`.
    #[must_use]
    pub fn home_dir(&self) -> Option<PathBuf> {
        self.home.clone().or_else(|| {
            std::env::var_os("HOME")
                .filter(|home| !home.is_empty())
                .map(PathBuf::from)
        })
    }

    /// The build tools' cache root: [`Self::cache_dir`], else `build-cache`
    /// under the temporary directory.
    #[must_use]
    pub fn cache_root(&self) -> PathBuf {
        self.cache_dir
            .clone()
            .unwrap_or_else(|| self.temp_dir.join("build-cache"))
    }

    /// The deny list these settings name: the one [`Self::freeze_deny_list`]
    /// kept, else built now.
    #[must_use]
    pub fn deny_list(&self) -> Arc<DenyList> {
        self.frozen
            .clone()
            .unwrap_or_else(|| Arc::new(self.build_deny_list()))
    }

    /// Build the deny list once and keep it: every later command, the host
    /// git and the file tools share it. Change the fields before, not after.
    pub fn freeze_deny_list(&mut self) -> Arc<DenyList> {
        let list = Arc::new(self.build_deny_list());
        self.frozen = Some(list.clone());
        list
    }

    fn build_deny_list(&self) -> DenyList {
        DenyList::for_session(
            self.home_dir().as_deref(),
            self.app_id.as_deref(),
            self.protect_credentials,
            self.deny_read.iter().cloned(),
        )
    }
}

/// One command to run.
#[derive(Clone, Debug)]
pub struct CommandSpec {
    pub command: String,
    pub cwd: WsPath,
    pub timeout: Option<Duration>,
    pub mode: SandboxMode,
    pub network: bool,
    /// Listen on and connect to the loopback addresses
    /// ([`SandboxRequest::loopback`]).
    pub loopback: bool,
}

/// What a command did.
#[derive(Clone, Debug, Serialize)]
#[allow(clippy::struct_excessive_bools)] // a report of independent facts
pub struct CommandOutput {
    /// `None` when killed by a signal or the timeout.
    pub exit_code: Option<i32>,
    pub timed_out: bool,
    pub stdout: String,
    pub stderr: String,
    pub stdout_truncated: bool,
    pub stderr_truncated: bool,
    /// Bytes the command wrote to each stream (all of them, kept or not).
    pub stdout_bytes: u64,
    pub stderr_bytes: u64,
    /// The spilled whole output, when it was over the session's budget:
    /// `read_output` pages it.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub output_id: Option<String>,
    pub duration_ms: u64,
    pub sandbox: SandboxMode,
    pub enforcement: Enforcement,
    /// What the enforcement leaves out, when the level alone does not say
    /// (Landlock only: credentials and the desktop app's data are not
    /// hidden).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enforcement_note: Option<&'static str>,
    /// Whether it ran under `/bin/sh -c` (compound) or as plain argv.
    pub shell: bool,
}

/// A stream read with its head and tail kept.
struct Captured {
    text: String,
    truncated: bool,
    total: usize,
}

/// What a reader has captured so far, shared with [`run`] so a reader
/// that never finishes (a grandchild holding the pipe) still leaves its
/// output behind.
#[derive(Default)]
struct Capture {
    head: Vec<u8>,
    tail: std::collections::VecDeque<u8>,
    total: usize,
    /// Kept bytes dropped from the middle (the head and tail are full).
    omitted: usize,
    /// Which whole lines are kept ([`KeepLine`]); `None`: every byte.
    keep: Option<KeepLine>,
    /// The line read so far, not yet judged.
    line: Vec<u8>,
    /// The rest of a line too long to hold is kept unjudged.
    long_line: bool,
}

/// Whether a whole line of output (with its `\n`) is kept: `run_tests`
/// leaves out what its parser never reads (cargo's artifact records) so
/// that it cannot push what it does read out of the kept head and tail.
pub type KeepLine = fn(&[u8]) -> bool;

/// The longest line a [`KeepLine`] judges; the rest of a longer one is
/// kept.
const MAX_JUDGED_LINE: usize = 1024 * 1024;

impl Capture {
    fn filtered(keep: Option<KeepLine>) -> Self {
        Self {
            keep,
            ..Self::default()
        }
    }

    fn push(&mut self, bytes: &[u8], cap: usize) {
        self.total += bytes.len();
        let Some(keep) = self.keep else {
            self.keep_bytes(bytes, cap);
            return;
        };
        for chunk in bytes.split_inclusive(|byte| *byte == b'\n') {
            let ends = chunk.ends_with(b"\n");
            if self.long_line {
                self.keep_bytes(chunk, cap);
                self.long_line = !ends;
                continue;
            }
            self.line.extend_from_slice(chunk);
            if ends {
                let line = std::mem::take(&mut self.line);
                if keep(&line) {
                    self.keep_bytes(&line, cap);
                }
            } else if self.line.len() > MAX_JUDGED_LINE {
                let line = std::mem::take(&mut self.line);
                self.keep_bytes(&line, cap);
                self.long_line = true;
            }
        }
    }

    /// The last line, which had no `\n`, judged at the end of the stream.
    fn finish(&mut self, cap: usize) {
        let line = std::mem::take(&mut self.line);
        if !line.is_empty() && self.keep.is_none_or(|keep| keep(&line)) {
            self.keep_bytes(&line, cap);
        }
    }

    fn keep_bytes(&mut self, bytes: &[u8], cap: usize) {
        let half = cap / 2;
        for byte in bytes {
            if self.head.len() < half {
                self.head.push(*byte);
            } else {
                self.tail.push_back(*byte);
                if self.tail.len() > cap - half {
                    self.tail.pop_front();
                    self.omitted += 1;
                }
            }
        }
    }

    /// The text so far; `cut_off` when the reader did not reach the end.
    /// Lines a [`KeepLine`] left out are not counted as omitted.
    fn render(&self, cut_off: bool) -> Captured {
        let omitted = self.omitted;
        let mut text = String::from_utf8_lossy(&self.head).into_owned();
        if omitted > 0 {
            let _ = write!(text, "\n… [{omitted} bytes omitted] …\n");
        }
        let tail: Vec<u8> = self.tail.iter().copied().collect();
        text.push_str(&String::from_utf8_lossy(&tail));
        if cut_off {
            text.push_str("\n… [output cut off: a process still held the stream] …\n");
        }
        Captured {
            text,
            truncated: omitted > 0 || cut_off,
            total: self.total,
        }
    }
}

type SharedCapture = Arc<Mutex<Capture>>;

async fn capture<R: AsyncRead + Unpin>(mut reader: R, cap: usize, into: SharedCapture) {
    let mut buffer = [0u8; 8192];
    while let Ok(read) = reader.read(&mut buffer).await {
        if read == 0 {
            break;
        }
        if let Ok(mut captured) = into.lock() {
            captured.push(&buffer[..read], cap);
        }
    }
    if let Ok(mut captured) = into.lock() {
        captured.finish(cap);
    }
}

/// Give both readers a bounded drain, then render what they captured. A
/// reader still running holds a pipe some escaped process keeps open: keep
/// what it read, stop waiting for the rest.
async fn drain(
    out: (&mut tokio::task::JoinHandle<()>, &SharedCapture),
    err: (&mut tokio::task::JoinHandle<()>, &SharedCapture),
) -> (Captured, Captured) {
    let _ = tokio::time::timeout(DRAIN_AFTER_KILL, async {
        let _ = (&mut *out.0).await;
        let _ = (&mut *err.0).await;
    })
    .await;
    let (out_cut, err_cut) = (!out.0.is_finished(), !err.0.is_finished());
    out.0.abort();
    err.0.abort();
    (rendered(out.1, out_cut), rendered(err.1, err_cut))
}

fn rendered(capture: &SharedCapture, cut_off: bool) -> Captured {
    capture.lock().map_or_else(
        |_| Captured {
            text: String::new(),
            truncated: true,
            total: 0,
        },
        |captured| captured.render(cut_off),
    )
}

/// The person's variables a command gets: the passthrough set (and the
/// host's `env_passthrough`), secret-looking names dropped. `run_command`
/// adds its own fixed settings (`TMPDIR`, `TERM`, …) on top. The handoff's
/// env harvest offers these (ADR-0034 decision 5).
#[must_use]
pub fn passthrough_environment(config: &ShellConfig) -> Vec<(String, String)> {
    let names = PASSTHROUGH
        .iter()
        .map(|name| (*name).to_owned())
        .chain(config.env_passthrough.iter().cloned());
    names
        .filter(|name| {
            let upper = name.to_ascii_uppercase();
            !SECRET_MARKERS.iter().any(|marker| upper.contains(marker))
        })
        .filter_map(|name| std::env::var(&name).ok().map(|value| (name, value)))
        .collect()
}

fn environment(config: &ShellConfig) -> Vec<(String, String)> {
    environment_from(config, passthrough_environment(config))
}

/// A background process's environment: a command's, and on a
/// pseudo-terminal `TERM=xterm-256color` without `NO_COLOR` (an
/// interactive program draws its screen; the model reads it rendered).
#[must_use]
pub fn process_environment(config: &ShellConfig, tty: bool) -> Vec<(String, String)> {
    let mut env = environment(config);
    if tty {
        env.retain(|(name, _)| name != "NO_COLOR");
        if let Some(term) = env.iter_mut().find(|(name, _)| name == "TERM") {
            "xterm-256color".clone_into(&mut term.1);
        }
    }
    env
}

/// [`environment`] over the given passed-through variables.
fn environment_from(config: &ShellConfig, passed: Vec<(String, String)>) -> Vec<(String, String)> {
    let mut out = passed;
    // PATH without relative or empty entries: a bare name never runs a
    // program from the workspace (the approval rules resolve it the same
    // way).
    if let Some(home) = &config.home {
        out.retain(|(name, _)| name != "HOME");
        out.push(("HOME".to_owned(), home.display().to_string()));
    }
    if let Some(entry) = out.iter_mut().find(|(name, _)| name == "PATH")
        && let Ok(joined) = std::env::join_paths(crate::command::search_path())
    {
        entry.1 = joined.to_string_lossy().into_owned();
    }
    out.extend(
        [
            ("TMPDIR", config.temp_dir.display().to_string()),
            ("TERM", "dumb".to_owned()),
            ("NO_COLOR", "1".to_owned()),
            ("PAGER", "cat".to_owned()),
            ("GIT_PAGER", "cat".to_owned()),
            ("GIT_TERMINAL_PROMPT", "0".to_owned()),
        ]
        .map(|(name, value)| (name.to_owned(), value)),
    );
    // Go's build cache under the home is not writable in the sandbox, and
    // go then misreports the standard library as missing; Gradle's and
    // Maven's caches there are not writable either. One cache root kept
    // across sessions holds them (a session does not start cold, and what
    // an online run fetched serves later offline runs), unless the host
    // passes its own (`GOCACHE`, `GRADLE_USER_HOME`, `-Dmaven.repo.local`
    // in `MAVEN_OPTS`).
    let cache = config.cache_root();
    if !out.iter().any(|(name, _)| name == "GOCACHE") {
        out.push(("GOCACHE".to_owned(), cache.join("go").display().to_string()));
    }
    if !out.iter().any(|(name, _)| name == "GRADLE_USER_HOME") {
        out.push((
            "GRADLE_USER_HOME".to_owned(),
            cache.join("gradle").display().to_string(),
        ));
    }
    let maven_repo = format!("-Dmaven.repo.local={}", cache.join("m2").display());
    match out.iter_mut().find(|(name, _)| name == "MAVEN_OPTS") {
        Some((_, opts)) if opts.contains("-Dmaven.repo.local=") => {}
        Some((_, opts)) if opts.trim().is_empty() => *opts = maven_repo,
        Some((_, opts)) => {
            opts.push(' ');
            opts.push_str(&maven_repo);
        }
        None => out.push(("MAVEN_OPTS".to_owned(), maven_repo)),
    }
    // A container host's workspace root that git must trust although
    // another uid owns it (empty on the desktop).
    let trust = config.sandbox.git_trust_env();
    if !trust.is_empty() {
        out.retain(|(name, _)| !crate::sandbox::is_git_config_env(name));
        out.extend(trust);
    }
    out
}

/// The sandbox request for a command in `workspace`.
///
/// # Errors
///
/// When the temporary directory or the build tools' cache cannot be
/// created.
pub fn sandbox_request(
    workspace: &Workspace,
    config: &ShellConfig,
    mode: SandboxMode,
    network: bool,
) -> ToolResult<SandboxRequest> {
    std::fs::create_dir_all(&config.temp_dir).map_err(|error| {
        ToolError::io("cannot create the session's temporary directory", &error)
    })?;
    let temp = std::fs::canonicalize(&config.temp_dir).map_err(|error| {
        ToolError::io("cannot resolve the session's temporary directory", &error)
    })?;
    // The build tools' cache root, kept across sessions, is writable too
    // (inside the denied data directory it is opened again, like the
    // temporary directory).
    let cache = config.cache_root();
    std::fs::create_dir_all(&cache)
        .map_err(|error| ToolError::io("cannot create the build tools' cache", &error))?;
    let cache = std::fs::canonicalize(&cache)
        .map_err(|error| ToolError::io("cannot resolve the build tools' cache", &error))?;
    let mut writable_roots = vec![workspace.root().to_path_buf(), temp];
    if !writable_roots.iter().any(|root| cache.starts_with(root)) {
        writable_roots.push(cache);
    }
    let deny_paths = config.deny_list().paths().to_vec();
    Ok(SandboxRequest {
        mode,
        network,
        writable_roots,
        protected: Vec::new(),
        git_roots: vec![workspace.root().to_path_buf()],
        deny_paths,
        deny_globs: workspace.deny_patterns().to_vec(),
        deny_root: Some(workspace.root().to_path_buf()),
        allow_listen: config.allow_listen,
        loopback: false,
        allow_keychain: config.allow_keychain,
    })
}

/// The Linux helper exits 125 with an `elitea sandbox:` message when the
/// kernel cannot give the confinement it reports: the command never ran.
fn helper_refusal(
    enforcement: Enforcement,
    exit_code: Option<i32>,
    stderr: &str,
) -> Option<ToolError> {
    (cfg!(target_os = "linux")
        && enforcement == Enforcement::Partial
        && exit_code == Some(125)
        && stderr.starts_with("elitea sandbox:"))
    .then(|| ToolError::new(ErrorCode::SandboxUnavailable, stderr.trim().to_owned()))
}

/// Run one command.
///
/// # Errors
///
/// A working directory that is not a directory in the workspace, a
/// sandbox that cannot be enforced, or a command that cannot start. A
/// command that runs and fails is not an error: see
/// [`CommandOutput::exit_code`].
pub async fn run(
    workspace: &Workspace,
    config: &ShellConfig,
    spec: &CommandSpec,
) -> ToolResult<CommandOutput> {
    run_checked(workspace, config, spec, true).await
}

/// How `spec` would run: the sandbox [`prepare`] chooses for it now. What
/// the session classifies a command by before asking about it.
///
/// # Errors
///
/// As [`run`], before anything runs.
pub fn plan(
    workspace: &Workspace,
    config: &ShellConfig,
    spec: &CommandSpec,
) -> ToolResult<Prepared> {
    let request = spec_request(workspace, config, spec)?;
    let prepared = prepare(&request, &["/usr/bin/true".to_owned()], &config.sandbox)?;
    Ok(Prepared {
        // Full access with the network is no sandbox, by request: what
        // the person allowed for it, not a sandbox falling short.
        hides_denied: prepared.hides_denied || request.is_unconfined(),
        ..prepared
    })
}

/// Whether `spec` would run unconfined now, as the rules judge it
/// ([`crate::approvals::ToolCall::unconfined`]): its sandbox does not hide
/// what the session denies, or it asks for the loopback where the sandbox
/// cannot keep it to the loopback addresses. `false` when the sandbox
/// cannot run at all (refused after the rules).
#[must_use]
pub fn runs_unconfined(workspace: &Workspace, config: &ShellConfig, spec: &CommandSpec) -> bool {
    plan(workspace, config, spec).is_ok_and(|prepared| {
        !prepared.hides_denied || (spec.loopback && !crate::sandbox::loopback_scoped(&prepared))
    })
}

/// The sandbox request for `spec` (its mode, network and loopback).
fn spec_request(
    workspace: &Workspace,
    config: &ShellConfig,
    spec: &CommandSpec,
) -> ToolResult<SandboxRequest> {
    let mut request = sandbox_request(workspace, config, spec.mode, spec.network)?;
    request.loopback = spec.loopback;
    Ok(request)
}

/// `spec` ready to spawn: its working directory checked, its argv (under
/// `/bin/sh -c` when compound) wrapped in the sandbox, and refused as
/// [`run_checked`] refuses. What `run_command` runs now and what a
/// background process starts from.
///
/// # Errors
///
/// As [`run_checked`], before anything runs.
pub fn prepare_command(
    workspace: &Workspace,
    config: &ShellConfig,
    spec: &CommandSpec,
    unhidden_approved: bool,
) -> ToolResult<PreparedCommand> {
    if workspace.stat(&spec.cwd)? != Some(EntryKind::Dir) {
        return Err(ToolError::invalid(format!(
            "`{}` is not a directory in the workspace",
            spec.cwd
        )));
    }
    let (argv, shell) = match analyse(&spec.command) {
        CommandShape::Simple(argv) => (argv, false),
        CommandShape::Compound { .. } => (
            vec!["/bin/sh".to_owned(), "-c".to_owned(), spec.command.clone()],
            true,
        ),
    };
    let request = spec_request(workspace, config, spec)?;
    let prepared = prepare(&request, &argv, &config.sandbox)?;
    refuse_unhidden(&request, &prepared, unhidden_approved)?;
    Ok(PreparedCommand {
        program: argv[0].clone(),
        prepared,
        shell,
    })
}

/// What [`prepare_command`] made of a command.
#[derive(Clone, Debug)]
pub struct PreparedCommand {
    /// The sandboxed program and its arguments.
    pub prepared: Prepared,
    /// Whether it runs under `/bin/sh -c` (compound).
    pub shell: bool,
    /// The program the command names (`/bin/sh` when compound).
    pub program: String,
}

/// The refusal [`run_checked`] makes: the sandbox chosen now does not hide
/// what `request` denies, and the command was not approved that way.
fn refuse_unhidden(
    request: &SandboxRequest,
    prepared: &Prepared,
    unhidden_approved: bool,
) -> ToolResult<()> {
    if prepared.hides_denied || request.is_unconfined() || unhidden_approved {
        return Ok(());
    }
    Err(ToolError::new(
        ErrorCode::SandboxUnavailable,
        format!(
            "the sandbox can no longer hide what this command may not read ({}); nothing \
             ran: run it again to be asked",
            prepared.note.unwrap_or("no sandbox")
        ),
    ))
}

/// [`run`], refusing (nothing runs) when the sandbox chosen now does not
/// hide what the request denies and `unhidden_approved` is false: the
/// command was approved as confined, and the sandbox changed since.
///
/// # Errors
///
/// As [`run`], and [`ErrorCode::SandboxUnavailable`] for that refusal.
pub async fn run_checked(
    workspace: &Workspace,
    config: &ShellConfig,
    spec: &CommandSpec,
    unhidden_approved: bool,
) -> ToolResult<CommandOutput> {
    run_checked_up_to(
        workspace,
        config,
        spec,
        unhidden_approved,
        config.max_timeout,
        None,
    )
    .await
}

/// [`run_checked`] with its timeout capped at `max_timeout` instead of
/// [`ShellConfig::max_timeout`] (`run_tests`:
/// [`ShellConfig::max_test_timeout`]), and only the stdout lines
/// `keep_stdout_line` keeps captured (all when `None`): the result's
/// `stdout_bytes` still counts every byte, and `stdout_truncated` says
/// only whether kept lines were cut.
///
/// # Errors
///
/// As [`run_checked`].
pub async fn run_checked_up_to(
    workspace: &Workspace,
    config: &ShellConfig,
    spec: &CommandSpec,
    unhidden_approved: bool,
    max_timeout: Duration,
    keep_stdout_line: Option<KeepLine>,
) -> ToolResult<CommandOutput> {
    let PreparedCommand {
        prepared,
        shell,
        program,
    } = prepare_command(workspace, config, spec, unhidden_approved)?;
    let timeout = spec
        .timeout
        .unwrap_or(config.default_timeout)
        .min(max_timeout);

    let mut command = tokio::process::Command::new(&prepared.program);
    command
        .args(&prepared.args)
        .current_dir(workspace.absolute(&spec.cwd))
        .env_clear()
        .envs(environment(config))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0)
        .kill_on_drop(true);
    let started = Instant::now();
    let mut child = command
        .spawn()
        .map_err(|error| ToolError::io(&format!("cannot start `{program}`"), &error))?;
    let group = child
        .id()
        .and_then(|id| i32::try_from(id).ok())
        .and_then(rustix::process::Pid::from_raw);
    // A caller that stops waiting (the future is dropped, e.g. a cloud
    // turn's take back) kills the whole group: `kill_on_drop` alone reaches
    // only the leader, and a build's children would keep writing.
    let mut group_guard = GroupKillOnDrop(group);
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| ToolError::new(ErrorCode::Io, "no stdout"))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| ToolError::new(ErrorCode::Io, "no stderr"))?;
    let cap = config.output_cap;
    let out_capture = Arc::new(Mutex::new(Capture::filtered(keep_stdout_line)));
    let err_capture = SharedCapture::default();
    let mut out_reader = tokio::spawn(capture(stdout, cap, Arc::clone(&out_capture)));
    let mut err_reader = tokio::spawn(capture(stderr, cap, Arc::clone(&err_capture)));

    let waited = tokio::time::timeout(timeout, child.wait()).await;
    let (status, timed_out) = if let Ok(status) = waited {
        (status.ok(), false)
    } else {
        if let Some(group) = group {
            let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
        }
        let _ = child.start_kill();
        (child.wait().await.ok(), true)
    };
    // Children left in the group may still hold the pipes: kill the group
    // after the leader exits too, then give the readers a bounded drain.
    group_guard.kill_now();
    if spec.mode != SandboxMode::ReadOnly {
        // It may have created a `.git` or a denied file.
        config.sandbox.masks.invalidate();
    }
    let (out, err) = drain(
        (&mut out_reader, &out_capture),
        (&mut err_reader, &err_capture),
    )
    .await;
    let exit_code = status
        .filter(|_| !timed_out)
        .and_then(|status| status.code());
    if let Some(refusal) = helper_refusal(prepared.enforcement, exit_code, &err.text) {
        return Err(refusal);
    }
    Ok(CommandOutput {
        exit_code,
        timed_out,
        stdout: out.text,
        stderr: err.text,
        stdout_truncated: out.truncated,
        stderr_truncated: err.truncated,
        stdout_bytes: out.total as u64,
        stderr_bytes: err.total as u64,
        output_id: None,
        duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
        sandbox: spec.mode,
        enforcement: prepared.enforcement,
        enforcement_note: prepared.note,
        shell,
    })
}

/// The person's login-shell environment, as editors read it (the handoff's
/// env harvest, ADR-0034 decision 5): `vars`, or none with a `notice` when
/// the shell could not be asked. Never an error: the harvest then offers
/// nothing.
#[derive(Default)]
pub struct LoginEnvironment {
    pub vars: Vec<(String, String)>,
    pub notice: Option<String>,
}

impl std::fmt::Debug for LoginEnvironment {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Names only: values may be secrets.
        f.debug_struct("LoginEnvironment")
            .field(
                "names",
                &self.vars.iter().map(|(n, _)| n).collect::<Vec<_>>(),
            )
            .field("notice", &self.notice)
            .finish()
    }
}

/// How much a login shell may print for its environment.
const LOGIN_ENV_CAP: usize = 1024 * 1024;

/// The login shell to ask: `$SHELL` when it is an absolute path to a file,
/// else `/bin/zsh` on macOS and `/bin/bash` elsewhere.
#[must_use]
pub fn login_shell() -> PathBuf {
    std::env::var_os("SHELL")
        .map(PathBuf::from)
        .filter(|shell| shell.is_absolute() && shell.is_file())
        .unwrap_or_else(|| {
            PathBuf::from(if cfg!(target_os = "macos") {
                "/bin/zsh"
            } else {
                "/bin/bash"
            })
        })
}

/// Ask `shell` (`-l -i -c`) for its environment: `env -0` between two
/// markers (so whatever the rc files print is ignored), from the home
/// directory, with no stdin, its own process group killed at `timeout`, at
/// most 1 MiB read. The shell inherits this process's environment, as a
/// terminal would. A timeout, a failed start, a nonzero exit or output
/// without both markers give no variables and a `notice`.
pub async fn login_environment(shell: &std::path::Path, timeout: Duration) -> LoginEnvironment {
    let nonce = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |since| since.as_nanos());
    let marker = format!("__ELITEA_ENV_{}_{nonce}__", std::process::id());
    let script = format!("printf '%s' '{marker}'; /usr/bin/env -0; printf '%s' '{marker}'");
    let failed = |why: String| LoginEnvironment {
        vars: Vec::new(),
        notice: Some(why),
    };
    let mut command = tokio::process::Command::new(shell);
    command
        .args(["-l", "-i", "-c", &script])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .process_group(0)
        .kill_on_drop(true);
    if let Some(home) = std::env::var_os("HOME").filter(|home| !home.is_empty()) {
        command.current_dir(home);
    }
    let Ok(mut child) = command.spawn() else {
        return failed(format!("{} could not be started", shell.display()));
    };
    let group = child
        .id()
        .and_then(|id| i32::try_from(id).ok())
        .and_then(rustix::process::Pid::from_raw);
    let Some(stdout) = child.stdout.take() else {
        return failed("the login shell gave no output".to_owned());
    };
    let read = async {
        let mut out = Vec::new();
        let _ = stdout
            .take(LOGIN_ENV_CAP as u64)
            .read_to_end(&mut out)
            .await;
        let status = child.wait().await.ok();
        (out, status)
    };
    let Ok((out, status)) = tokio::time::timeout(timeout, read).await else {
        if let Some(group) = group {
            let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
        }
        return failed(format!(
            "the login shell did not answer in {}s",
            timeout.as_secs()
        ));
    };
    if let Some(group) = group {
        let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
    }
    if !status.is_some_and(|status| status.success()) {
        return failed("the login shell exited with an error".to_owned());
    }
    let marker = marker.as_bytes();
    let find = |from: usize| {
        out[from..]
            .windows(marker.len())
            .position(|w| w == marker)
            .map(|at| at + from)
    };
    let (Some(start), Some(end)) = (find(0), find(0).and_then(|s| find(s + marker.len()))) else {
        return failed("the login shell printed no environment".to_owned());
    };
    let vars = out[start + marker.len()..end]
        .split(|byte| *byte == 0)
        .filter_map(|entry| {
            let at = entry.iter().position(|byte| *byte == b'=')?;
            let name = std::str::from_utf8(&entry[..at]).ok()?;
            let value = std::str::from_utf8(&entry[at + 1..]).ok()?;
            (!name.is_empty()).then(|| (name.to_owned(), value.to_owned()))
        })
        .collect();
    LoginEnvironment { vars, notice: None }
}

/// Kills a command's process group when dropped.
struct GroupKillOnDrop(Option<rustix::process::Pid>);

impl GroupKillOnDrop {
    /// Kill the group now, once: a later drop must not signal a reused id.
    fn kill_now(&mut self) {
        if let Some(group) = self.0.take() {
            let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
        }
    }
}

impl Drop for GroupKillOnDrop {
    fn drop(&mut self) {
        self.kill_now();
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use super::{CommandSpec, ShellConfig, run};
    use crate::policy::SandboxMode;
    use crate::workspace::{Intent, Workspace, WsPath};

    fn setup() -> (tempfile::TempDir, Workspace, ShellConfig) {
        let dir = tempfile::tempdir().expect("dir");
        std::fs::create_dir(dir.path().join("ws")).expect("ws");
        let workspace = Workspace::open(&dir.path().join("ws"), &[]).expect("workspace");
        let mut config = ShellConfig::new(dir.path().join("tmp"));
        config.sandbox.allow_unenforced = true;
        (dir, workspace, config)
    }

    fn spec(command: &str, mode: SandboxMode) -> CommandSpec {
        CommandSpec {
            command: command.to_owned(),
            cwd: WsPath::root(),
            timeout: None,
            mode,
            network: false,
            loopback: false,
        }
    }

    /// In a container with a trusted root, a workspace command's git
    /// trusts exactly that root (`GIT_CONFIG_COUNT`); off (the desktop, or
    /// no root), nothing is added.
    #[test]
    fn a_container_root_reaches_the_commands_git_config_only_when_on() {
        let (_dir, _workspace, mut config) = setup();
        let git_env = |config: &ShellConfig| -> Vec<(String, String)> {
            super::environment(config)
                .into_iter()
                .filter(|(name, _)| name.starts_with("GIT_CONFIG_"))
                .collect()
        };
        assert_eq!(git_env(&config), [] as [(String, String); 0]);
        config.sandbox.git_safe_directory = Some("/workspace".into());
        assert!(git_env(&config).is_empty(), "not a container: unchanged");
        config.sandbox.container = true;
        assert_eq!(
            git_env(&config),
            [
                ("GIT_CONFIG_COUNT", "1"),
                ("GIT_CONFIG_KEY_0", "safe.directory"),
                ("GIT_CONFIG_VALUE_0", "/workspace"),
            ]
            .map(|(name, value)| (name.to_owned(), value.to_owned()))
        );
        config.sandbox.git_safe_directory = None;
        assert_eq!(git_env(&config), [] as [(String, String); 0]);
    }

    /// With the trust env on, every passed-through git config variable is
    /// dropped (a stale `KEY_1` or `PARAMETERS` cannot ride along); off,
    /// the passthrough is left as it was.
    #[test]
    fn the_trust_env_replaces_every_passed_through_git_config_variable() {
        let (_dir, _workspace, mut config) = setup();
        let passed: Vec<(String, String)> = [
            ("GIT_CONFIG_COUNT", "2"),
            ("GIT_CONFIG_KEY_1", "core.sshCommand"),
            ("GIT_CONFIG_VALUE_1", "evil"),
            ("GIT_CONFIG_PARAMETERS", "'core.pager'='less'"),
            ("LANG", "C.UTF-8"),
        ]
        .map(|(name, value)| (name.to_owned(), value.to_owned()))
        .to_vec();
        let git_env = |config: &ShellConfig| -> Vec<(String, String)> {
            super::environment_from(config, passed.clone())
                .into_iter()
                .filter(|(name, _)| name.starts_with("GIT_CONFIG_"))
                .collect()
        };
        assert_eq!(git_env(&config).len(), 4, "off: unchanged");
        config.sandbox.container = true;
        config.sandbox.git_safe_directory = Some("/workspace".into());
        assert_eq!(
            git_env(&config),
            [
                ("GIT_CONFIG_COUNT", "1"),
                ("GIT_CONFIG_KEY_0", "safe.directory"),
                ("GIT_CONFIG_VALUE_0", "/workspace"),
            ]
            .map(|(name, value)| (name.to_owned(), value.to_owned()))
        );
        assert!(
            super::environment_from(&config, passed.clone())
                .iter()
                .any(|(name, _)| name == "LANG")
        );
    }

    /// A capture with a line filter keeps only the lines it keeps, judged
    /// whole however the reads split them: thousands of artifact records
    /// cannot push the test results out of a small cap, and what was left
    /// out on purpose is not reported as truncated (#1224).
    #[test]
    fn a_line_filter_drops_lines_before_the_cap() {
        let artifact = format!(
            "{{\"reason\":\"compiler-artifact\",\"filenames\":[\"{}\"]}}\n",
            "x".repeat(2000)
        );
        let mut stream = String::new();
        for n in 0..5000 {
            if n == 2500 {
                stream.push_str("test result: ok. 3 passed; 0 failed; 0 ignored\n");
            }
            stream.push_str(&artifact);
        }
        stream.push_str("test tests::adds ... ok\ntest result: ok. 1 passed; 0 failed; 0 ignored");
        let cap = 64 * 1024;
        let mut capture = super::Capture::filtered(Some(crate::test_report::cargo_keeps_line));
        for chunk in stream.as_bytes().chunks(777) {
            capture.push(chunk, cap);
        }
        capture.finish(cap);
        let captured = capture.render(false);
        assert!(!captured.truncated, "{}", captured.text.len());
        assert_eq!(captured.total, stream.len());
        assert_eq!(
            captured.text,
            "test result: ok. 3 passed; 0 failed; 0 ignored\ntest tests::adds ... ok\ntest \
             result: ok. 1 passed; 0 failed; 0 ignored"
        );
        // Unfiltered, the same stream is cut.
        let mut plain = super::Capture::default();
        plain.push(stream.as_bytes(), cap);
        let plain = plain.render(false);
        assert!(plain.truncated);
        assert!(plain.text.contains("bytes omitted"));
        assert!(!plain.text.contains("3 passed"), "lost in the middle");
    }

    /// Go's, Gradle's and Maven's caches go to one cache root kept across
    /// sessions (the sandbox cannot write the home's), unless the host
    /// passes its own; a host's other `MAVEN_OPTS` are kept.
    #[test]
    fn build_tool_caches_share_one_root_across_sessions() {
        let (dir, workspace, mut config) = setup();
        let value = |env: &[(String, String)], name: &str| {
            env.iter()
                .find(|(key, _)| key == name)
                .map(|(_, value)| value.clone())
        };
        // Without a host root, under the temporary directory (already
        // writable: no extra root).
        let fallback = config.temp_dir.join("build-cache");
        assert_eq!(
            value(&super::environment_from(&config, Vec::new()), "GOCACHE"),
            Some(fallback.join("go").display().to_string())
        );
        let request =
            super::sandbox_request(&workspace, &config, SandboxMode::WorkspaceWrite, false)
                .expect("request");
        assert_eq!(request.writable_roots.len(), 2);
        // Two sessions over one data directory name the same root, and the
        // sandbox may write it.
        let cache = dir.path().join("data/build-cache");
        config.cache_dir = Some(cache.clone());
        let mut other = ShellConfig::new(dir.path().join("tmp-other"));
        other.cache_dir = Some(cache.clone());
        let env = super::environment_from(&config, Vec::new());
        let other_env = super::environment_from(&other, Vec::new());
        for name in ["GOCACHE", "GRADLE_USER_HOME", "MAVEN_OPTS"] {
            assert_eq!(value(&env, name), value(&other_env, name), "{name}");
        }
        assert_eq!(
            value(&env, "GOCACHE"),
            Some(cache.join("go").display().to_string())
        );
        assert_eq!(
            value(&env, "GRADLE_USER_HOME"),
            Some(cache.join("gradle").display().to_string())
        );
        let repo = format!("-Dmaven.repo.local={}", cache.join("m2").display());
        assert_eq!(value(&env, "MAVEN_OPTS"), Some(repo.clone()));
        let request =
            super::sandbox_request(&workspace, &config, SandboxMode::WorkspaceWrite, false)
                .expect("request");
        let canonical = std::fs::canonicalize(&cache).expect("created");
        assert_eq!(request.writable_roots.last(), Some(&canonical));
        let passed = |pairs: &[(&str, &str)]| -> Vec<(String, String)> {
            pairs
                .iter()
                .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
                .collect()
        };
        let env = super::environment_from(
            &config,
            passed(&[
                ("GRADLE_USER_HOME", "/host/gradle"),
                ("MAVEN_OPTS", "-Xmx1g -Dmaven.repo.local=/host/m2"),
            ]),
        );
        assert_eq!(
            value(&env, "GRADLE_USER_HOME").as_deref(),
            Some("/host/gradle")
        );
        assert_eq!(
            value(&env, "MAVEN_OPTS").as_deref(),
            Some("-Xmx1g -Dmaven.repo.local=/host/m2")
        );
        assert_eq!(
            env.iter()
                .filter(|(name, _)| name == "GRADLE_USER_HOME")
                .count(),
            1
        );
        let env = super::environment_from(&config, passed(&[("MAVEN_OPTS", "-Xmx1g")]));
        assert_eq!(value(&env, "MAVEN_OPTS"), Some(format!("-Xmx1g {repo}")));
    }

    #[tokio::test]
    async fn runs_in_the_workspace_with_a_scrubbed_environment() {
        let (_dir, workspace, config) = setup();
        let output = run(&workspace, &config, &spec("env", SandboxMode::FullAccess))
            .await
            .expect("run");
        assert_eq!(output.exit_code, Some(0));
        assert!(output.stdout.contains("TMPDIR="));
        assert!(output.stdout.contains("TERM=dumb"));
        assert!(
            !output.stdout.contains("CARGO_PKG_NAME"),
            "the host's variables are dropped"
        );
        let pwd = run(&workspace, &config, &spec("pwd", SandboxMode::FullAccess))
            .await
            .expect("run");
        assert_eq!(pwd.stdout.trim(), workspace.root().display().to_string());
        assert!(!pwd.shell);
    }

    #[tokio::test]
    async fn compound_commands_run_under_sh_and_exit_codes_come_back() {
        let (_dir, workspace, config) = setup();
        let output = run(
            &workspace,
            &config,
            &spec("echo one && exit 3", SandboxMode::FullAccess),
        )
        .await
        .expect("run");
        assert!(output.shell);
        assert_eq!(output.stdout.trim(), "one");
        assert_eq!(output.exit_code, Some(3));
    }

    #[tokio::test]
    async fn timeouts_kill_the_whole_group() {
        let (_dir, workspace, config) = setup();
        let mut slow = spec("sleep 30 & sleep 30; wait", SandboxMode::FullAccess);
        slow.timeout = Some(Duration::from_millis(300));
        let started = std::time::Instant::now();
        let output = run(&workspace, &config, &slow).await.expect("run");
        assert!(output.timed_out);
        assert_eq!(output.exit_code, None);
        assert!(started.elapsed() < Duration::from_secs(10));
    }

    /// A grandchild that leaves the process group and keeps stdout open
    /// does not cost the output read before it: the readers are cut off
    /// after the drain, what they read is kept and marked truncated.
    #[tokio::test]
    async fn output_read_before_a_stuck_pipe_is_kept() {
        if !std::path::Path::new("/usr/bin/perl").is_file() {
            return;
        }
        let (_dir, workspace, config) = setup();
        let started = std::time::Instant::now();
        let output = run(
            &workspace,
            &config,
            &spec(
                "echo before; echo oops >&2; /usr/bin/perl -MPOSIX -e 'POSIX::setsid(); sleep 6' & sleep 1",
                SandboxMode::FullAccess,
            ),
        )
        .await
        .expect("run");
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "the drain is bounded"
        );
        assert!(output.stdout.starts_with("before\n"), "{:?}", output.stdout);
        assert!(output.stdout_truncated);
        assert!(output.stderr.starts_with("oops\n"), "{:?}", output.stderr);
        assert!(output.stderr_truncated);
    }

    #[tokio::test]
    async fn output_is_capped_keeping_head_and_tail() {
        let (_dir, workspace, mut config) = setup();
        config.output_cap = 64;
        let output = run(
            &workspace,
            &config,
            &spec("seq 1 10000", SandboxMode::FullAccess),
        )
        .await
        .expect("run");
        assert!(output.stdout_truncated);
        assert!(output.stdout.starts_with("1\n2\n"));
        assert!(output.stdout.trim_end().ends_with("10000"));
        assert!(output.stdout.contains("bytes omitted"));
    }

    /// A command printing about 1 MB answers its head and tail within the
    /// 6k-token budget and an output id; `read_output` pages the whole.
    #[tokio::test]
    async fn large_output_spills_and_pages() {
        use crate::budget::{self, Budgets, SpillStore};
        let (dir, workspace, config) = setup();
        let mut output = run(
            &workspace,
            &config,
            &spec("seq 1 160000", SandboxMode::FullAccess),
        )
        .await
        .expect("run");
        assert!(output.stdout_bytes >= 1_000_000, "{}", output.stdout_bytes);
        assert!(!output.stdout_truncated, "the capture keeps it all");
        let spill = SpillStore::new(&dir.path().join("data"), "s");
        budget::shape_command(
            &mut output,
            budget::bytes(Budgets::default().run_command),
            &spill,
        );
        let shown = output.stdout.len() + output.stderr.len();
        assert!(budget::estimate_tokens(shown) <= 6_000, "{shown}");
        assert!(output.stdout_truncated);
        assert!(output.stdout.starts_with("1\n2\n"), "the head");
        assert!(output.stdout.ends_with("160000\n"), "the tail");
        let id = output.output_id.clone().expect("spilled");
        assert!(output.stdout.contains(&id), "the note names the id");
        let value = serde_json::to_value(&output).expect("json");
        assert_eq!(value["output_id"], id.as_str());
        assert_eq!(value["stdout_bytes"], output.stdout_bytes);
        let page_budget = budget::bytes(Budgets::default().read_file);
        let first =
            budget::read_output(&spill, serde_json::json!({ "output_id": id }), page_budget)
                .expect("page");
        assert!(
            first["content"]
                .as_str()
                .expect("content")
                .starts_with("     1\t1\n")
        );
        assert_eq!(first["total_lines"], 160_000);
        let next = first["next_offset"].as_u64().expect("more");
        let second = budget::read_output(
            &spill,
            serde_json::json!({ "output_id": id, "offset": next }),
            page_budget,
        )
        .expect("page");
        assert_eq!(second["start_line"], next);
        let last = budget::read_output(
            &spill,
            serde_json::json!({ "output_id": id, "offset": 160_000 }),
            page_budget,
        )
        .expect("page");
        assert_eq!(last["content"], "160000\t160000\n");
        assert_eq!(last["truncated"], false);
    }

    /// The session's data directory sits under the desktop app's own data
    /// directory, which is denied: the temporary directory inside it is
    /// opened again (Seatbelt) or bound over the mask (bubblewrap), and
    /// the home override decides which credentials are denied.
    #[test]
    fn the_temp_dir_under_the_denied_app_data_stays_writable() {
        use crate::sandbox::{bubblewrap, seatbelt};
        let dir = tempfile::tempdir().expect("dir");
        let base = std::fs::canonicalize(dir.path()).expect("canonical");
        let home = base.join("home");
        let app = home.join("Library/Application Support/ai.elitea.desktop");
        let data = app.join("workspaces/w1");
        let temp = data.join("tmp/w1");
        std::fs::create_dir_all(&data).expect("data");
        std::fs::create_dir(base.join("ws")).expect("ws");
        let workspace = Workspace::open(&base.join("ws"), &[]).expect("workspace");
        let mut config = ShellConfig::new(temp.clone());
        config.home = Some(home.clone());
        config.app_id = Some("ai.elitea.desktop".to_owned());
        config.deny_read.push(data.clone());
        let request =
            super::sandbox_request(&workspace, &config, SandboxMode::WorkspaceWrite, false)
                .expect("request");
        assert!(
            request.deny_paths.contains(&app),
            "{:?}",
            request.deny_paths
        );
        assert!(request.deny_paths.contains(&data));
        assert_eq!(request.writable_roots[1], temp);

        let (text, params) = seatbelt::profile(&request);
        assert!(params.contains(&("WRITABLE_ROOT_1".to_owned(), temp.display().to_string())));
        let deny = text
            .find("(deny file-read-data file-write*")
            .expect("denial");
        let reopen = text
            .find("(allow file-write* (subpath (param \"WRITABLE_ROOT_1\")))")
            .expect("the temporary directory is opened again");
        assert!(reopen > deny, "the reopening comes after the denial");
        assert!(
            text.contains(
                "(allow file-read* file-read-data (subpath (param \"WRITABLE_ROOT_1\")))"
            )
        );

        let masks = bubblewrap::Masks::discover(&request);
        assert!(masks.hide_dirs.contains(&app));
        let args = bubblewrap::args(&request, &masks, &["true".to_owned()]).join(" ");
        let hide = args
            .find(&format!("--tmpfs {}", app.display()))
            .expect("app data hidden");
        let rebind = args
            .rfind(&format!("--bind {0} {0}", temp.display()))
            .expect("temp bound");
        assert!(rebind > hide, "bound over the mask");
    }

    #[tokio::test]
    async fn the_working_directory_must_be_a_workspace_directory() {
        let (_dir, workspace, config) = setup();
        std::fs::write(workspace.root().join("f"), "x").expect("seed");
        let mut in_file = spec("pwd", SandboxMode::FullAccess);
        in_file.cwd = workspace.resolve("f", Intent::Read).expect("path");
        assert!(run(&workspace, &config, &in_file).await.is_err());
        let mut missing = spec("pwd", SandboxMode::FullAccess);
        missing.cwd = workspace.resolve("nope", Intent::Read).expect("path");
        assert!(run(&workspace, &config, &missing).await.is_err());
    }

    fn fake_shell(dir: &std::path::Path, name: &str, body: &str) -> std::path::PathBuf {
        use std::os::unix::fs::PermissionsExt as _;
        let path = dir.join(name);
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).expect("write");
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755)).expect("chmod");
        path
    }

    #[tokio::test]
    async fn the_login_shell_environment_is_read_between_markers() {
        let dir = tempfile::tempdir().expect("dir");
        // An rc file that exports a variable and prints noise; `$4` is the
        // `-c` script (`-l -i -c <script>`).
        let shell = fake_shell(
            dir.path(),
            "zsh",
            "export DATABASE_URL='postgres://u:p@h/db'\nexport MULTI='a\nb'\necho rc noise\nexec /bin/sh -c \"$4\"",
        );
        let env = super::login_environment(&shell, Duration::from_secs(10)).await;
        assert!(env.notice.is_none(), "{:?}", env.notice);
        let get = |name: &str| {
            env.vars
                .iter()
                .find(|(n, _)| n == name)
                .map(|(_, v)| v.clone())
        };
        assert_eq!(get("DATABASE_URL").as_deref(), Some("postgres://u:p@h/db"));
        assert_eq!(get("MULTI").as_deref(), Some("a\nb"));
        assert!(
            !format!("{env:?}").contains("postgres"),
            "Debug shows names only"
        );
    }

    #[tokio::test]
    async fn a_failing_or_slow_login_shell_gives_nothing_and_a_notice() {
        let dir = tempfile::tempdir().expect("dir");
        for (name, body, timeout) in [
            ("exits", "exit 3", Duration::from_secs(10)),
            ("hangs", "sleep 30", Duration::from_millis(300)),
            ("babbles", "echo no markers here", Duration::from_secs(10)),
        ] {
            let shell = fake_shell(dir.path(), name, body);
            let started = std::time::Instant::now();
            let env = super::login_environment(&shell, timeout).await;
            assert!(env.vars.is_empty(), "{name}");
            assert!(env.notice.is_some(), "{name}");
            assert!(
                started.elapsed() < Duration::from_secs(5),
                "{name} was bounded"
            );
        }
        let missing =
            super::login_environment(&dir.path().join("none"), Duration::from_secs(1)).await;
        assert!(missing.vars.is_empty() && missing.notice.is_some());
    }
}
