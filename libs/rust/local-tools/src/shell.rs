//! Running one command: working directory inside the workspace, a scrubbed
//! environment, a timeout that kills the whole process group, output caps,
//! and the OS sandbox around it. No PTY yet.

use std::fmt::Write as _;
use std::path::PathBuf;
use std::process::Stdio;
use std::time::{Duration, Instant};

use serde::Serialize;
use tokio::io::{AsyncRead, AsyncReadExt};

use crate::command::{CommandShape, analyse};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::policy::SandboxMode;
use crate::sandbox::{Enforcement, SandboxConfig, SandboxRequest, credential_paths, prepare};
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

/// The host's command settings.
#[derive(Clone, Debug)]
pub struct ShellConfig {
    pub default_timeout: Duration,
    pub max_timeout: Duration,
    /// Bytes kept per stream (half from the start, half from the end).
    pub output_cap: usize,
    /// More variable names to pass through (never secret-looking ones).
    pub env_passthrough: Vec<String>,
    /// The session's temporary directory (`TMPDIR`; writable in
    /// workspace-write).
    pub temp_dir: PathBuf,
    pub sandbox: SandboxConfig,
    /// More files or directories commands may not read (the host's data
    /// directory: the session adds it); see
    /// [`SandboxRequest::deny_paths`].
    pub deny_read: Vec<PathBuf>,
    /// Deny reading [`credential_paths`] under `HOME` (on by default).
    pub protect_credentials: bool,
    /// Let commands with the network on listen for connections (off by
    /// default).
    pub allow_listen: bool,
    /// Let commands reach the keychain's services (off by default).
    pub allow_keychain: bool,
    /// The global git config the host's git reads (`None`: the person's,
    /// from `HOME`); for hosts that keep it elsewhere and for tests.
    pub git_global_config: Option<PathBuf>,
}

impl ShellConfig {
    /// Defaults: 2 minutes (at most 10), 64 KiB per stream.
    #[must_use]
    pub fn new(temp_dir: PathBuf) -> Self {
        Self {
            default_timeout: Duration::from_mins(2),
            max_timeout: Duration::from_mins(10),
            output_cap: 64 * 1024,
            env_passthrough: Vec::new(),
            temp_dir,
            sandbox: SandboxConfig::default(),
            deny_read: Vec::new(),
            protect_credentials: true,
            allow_listen: false,
            allow_keychain: false,
            git_global_config: None,
        }
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
    pub duration_ms: u64,
    pub sandbox: SandboxMode,
    pub enforcement: Enforcement,
    /// Whether it ran under `/bin/sh -c` (compound) or as plain argv.
    pub shell: bool,
}

/// A stream read with its head and tail kept.
struct Captured {
    text: String,
    truncated: bool,
}

async fn capture<R: AsyncRead + Unpin>(mut reader: R, cap: usize) -> Captured {
    let half = cap / 2;
    let mut head = Vec::new();
    let mut tail = std::collections::VecDeque::new();
    let mut total = 0usize;
    let mut buffer = [0u8; 8192];
    while let Ok(read) = reader.read(&mut buffer).await {
        if read == 0 {
            break;
        }
        total += read;
        for byte in &buffer[..read] {
            if head.len() < half {
                head.push(*byte);
            } else {
                tail.push_back(*byte);
                if tail.len() > cap - half {
                    tail.pop_front();
                }
            }
        }
    }
    let truncated = total > head.len() + tail.len();
    let mut text = String::from_utf8_lossy(&head).into_owned();
    if truncated {
        let _ = write!(
            text,
            "\n… [{} bytes omitted] …\n",
            total - head.len() - tail.len()
        );
    }
    let tail: Vec<u8> = tail.into_iter().collect();
    text.push_str(&String::from_utf8_lossy(&tail));
    Captured { text, truncated }
}

fn environment(config: &ShellConfig) -> Vec<(String, String)> {
    let names = PASSTHROUGH
        .iter()
        .map(|name| (*name).to_owned())
        .chain(config.env_passthrough.iter().cloned());
    let mut out: Vec<(String, String)> = names
        .filter(|name| {
            let upper = name.to_ascii_uppercase();
            !SECRET_MARKERS.iter().any(|marker| upper.contains(marker))
        })
        .filter_map(|name| std::env::var(&name).ok().map(|value| (name, value)))
        .collect();
    // PATH without relative or empty entries: a bare name never runs a
    // program from the workspace (the approval rules resolve it the same
    // way).
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
    out
}

/// The sandbox request for a command in `workspace`.
///
/// # Errors
///
/// When the temporary directory cannot be created.
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
    let mut deny_paths: Vec<PathBuf> = config
        .deny_read
        .iter()
        .map(|path| std::fs::canonicalize(path).unwrap_or_else(|_| path.clone()))
        .collect();
    if config.protect_credentials
        && let Some(home) = std::env::var_os("HOME").filter(|home| !home.is_empty())
    {
        deny_paths.extend(credential_paths(std::path::Path::new(&home)));
    }
    Ok(SandboxRequest {
        mode,
        network,
        writable_roots: vec![workspace.root().to_path_buf(), temp],
        protected: Vec::new(),
        git_roots: vec![workspace.root().to_path_buf()],
        deny_paths,
        deny_globs: workspace.deny_patterns().to_vec(),
        deny_root: Some(workspace.root().to_path_buf()),
        allow_listen: config.allow_listen,
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
    let request = sandbox_request(workspace, config, spec.mode, spec.network)?;
    let prepared = prepare(&request, &argv, &config.sandbox)?;
    let timeout = spec
        .timeout
        .unwrap_or(config.default_timeout)
        .min(config.max_timeout);

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
        .map_err(|error| ToolError::io(&format!("cannot start `{}`", argv[0]), &error))?;
    let group = child
        .id()
        .and_then(|id| i32::try_from(id).ok())
        .and_then(rustix::process::Pid::from_raw);
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| ToolError::new(ErrorCode::Io, "no stdout"))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| ToolError::new(ErrorCode::Io, "no stderr"))?;
    let cap = config.output_cap;
    let readers =
        tokio::spawn(async move { tokio::join!(capture(stdout, cap), capture(stderr, cap)) });

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
    if let Some(group) = group {
        let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
    }
    if spec.mode != SandboxMode::ReadOnly {
        // It may have created a `.git` or a denied file.
        config.sandbox.masks.invalidate();
    }
    let (out, err) = match tokio::time::timeout(DRAIN_AFTER_KILL, readers).await {
        Ok(Ok(captured)) => captured,
        _ => (
            Captured {
                text: String::new(),
                truncated: true,
            },
            Captured {
                text: String::new(),
                truncated: true,
            },
        ),
    };
    let exit_code = if timed_out {
        None
    } else {
        status.and_then(|status| status.code())
    };
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
        duration_ms: u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
        sandbox: spec.mode,
        enforcement: prepared.enforcement,
        shell,
    })
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
        }
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
}
