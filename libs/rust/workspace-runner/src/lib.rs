//! The workspace runner (ADR-0034 decision 9).
//!
//! It runs inside the cloud executor's sandbox container, never in the
//! worker. The worker starts it with one of three subcommands, through the
//! sandbox helper protocol:
//!
//! * `unpack`: read a workspace archive (`elitea_agent_runtime::workspace_archive`)
//!   from stdin into the empty workspace, then record it as the commit
//!   `git_status`, `git_diff` and `git_log` compare against;
//! * `serve`: answer tool calls, one JSON object per line on stdin
//!   (`{"tool","call_id","args"}`), one JSON result per line on stdout, for
//!   the life of the turn;
//! * `pack`: write the workspace (without `.git`) to stdout as an archive.
//!
//! The tools are `elitea-local-tools`' own, at the `container` enforcement
//! level: the container is the boundary, so commands run unwrapped. The tool
//! set is [`WORKSPACE_TOOLS`]: `git_commit` is refused here too, the worker
//! commits at the end of the turn. No token, store route or credential is
//! ever passed to this process.

#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

use std::path::{Path, PathBuf};
use std::sync::{Arc, OnceLock};

use async_trait::async_trait;
use elitea_agent_runtime::host::{ApprovalChannel, ApprovalOutcome, ApprovalRequest, HostError};
use elitea_agent_runtime::workspace_archive::{self, ArchiveError, ArchiveLimits, ArchiveStats};
use elitea_agent_runtime::workspace_tools::WORKSPACE_TOOLS;
use elitea_local_tools::approvals::{
    MemoryChoices, PAYLOAD_KEY, RulesEngine, ToolCall, ToolKind, Verdict, WorkspaceSettings,
};
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::session::{LocalSession, SessionConfig};
use elitea_local_tools::shell::ShellConfig;
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::io::{AsyncBufRead, AsyncBufReadExt as _, AsyncWrite, AsyncWriteExt as _};

/// One request line may be at most this long (a `write_file` of a large
/// file included).
pub const MAX_REQUEST_LINE_BYTES: usize = 32 * 1024 * 1024;

/// What the worker decides for one run (from the effective policy).
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq)]
#[serde(deny_unknown_fields, default)]
pub struct RunnerPolicy {
    pub shell: bool,
    pub network: bool,
    pub command_allow: Vec<String>,
    pub command_deny: Vec<String>,
}

/// Where the runner works.
#[derive(Clone, Debug)]
pub struct RunnerConfig {
    /// The workspace volume (`/workspace`).
    pub root: PathBuf,
    /// Scratch for the session (checkpoint data, `TMPDIR`), outside the
    /// workspace.
    pub data_dir: PathBuf,
    pub policy: RunnerPolicy,
    /// The thread's harvested env (contract 6.4), for `run_command` only.
    /// Read from the first stdin line, never from the process environment
    /// or the arguments.
    pub env: Vec<(String, String)>,
}

/// The `serve --env-stdin` handshake: the first line is `{"env":{...}}`.
///
/// # Errors
///
/// The line is not that object.
pub fn parse_env_line(line: &str) -> Result<Vec<(String, String)>, String> {
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Handshake {
        env: std::collections::BTreeMap<String, String>,
    }
    serde_json::from_str::<Handshake>(line)
        .map(|handshake| handshake.env.into_iter().collect())
        .map_err(|_| "runner.bad_env_handshake".to_owned())
}

/// The call ids a person approved through the server's HITL interrupt
/// (ADR-0034 decision 9): the worker sends `"approved": true` on the
/// request line of exactly the call the approval names, never from model
/// arguments. One runner process serves one turn, so a process-wide set is
/// the turn's set.
static APPROVED: std::sync::Mutex<Vec<String>> = std::sync::Mutex::new(Vec::new());

fn approved(call_id: &str) -> bool {
    APPROVED
        .lock()
        .is_ok_and(|approved| approved.iter().any(|seen| seen == call_id))
}

/// The approval channel of a cloud run.
///
/// The project setting already decided the network (decision 9: network
/// use is not an escaping action in the cloud), and the container is the
/// boundary for `full-access`. So a command the rules ask about only for
/// those reasons is approved. A destructive or escaping command (the
/// ADR-0029 addendum item 3 list, [`classify_command`]) is asked by the
/// worker as a server HITL interrupt BEFORE it is sent here; it arrives
/// approved, and is run. Anything else the rules ask about is deferred,
/// which the tool reports to the model as waiting for approval.
#[derive(Default)]
pub struct CloudPrompt {
    engine: OnceLock<Arc<RulesEngine>>,
}

impl CloudPrompt {
    fn bind(&self, engine: Arc<RulesEngine>) {
        let _ = self.engine.set(engine);
    }
}

#[async_trait]
impl ApprovalChannel for CloudPrompt {
    async fn request(&self, request: ApprovalRequest) -> Result<ApprovalOutcome, HostError> {
        let call = request
            .payload
            .get(PAYLOAD_KEY)
            .cloned()
            .and_then(|value| serde_json::from_value::<ToolCall>(value).ok());
        if let (Some(engine), Some(call)) = (self.engine.get(), call)
            && call.tool == ToolKind::RunCommand
        {
            let mut contained = call.clone();
            contained.network = false;
            if contained.sandbox == Some(SandboxMode::FullAccess) {
                contained.sandbox = Some(SandboxMode::WorkspaceWrite);
            }
            let policy = engine.policy();
            if (!call.network || policy.network)
                && (approved(&request.subject)
                    || engine.decide(&contained).verdict == Verdict::Allow)
            {
                return Ok(ApprovalOutcome::Decided {
                    action: "approve".to_owned(),
                    value: json!({
                        "reason": "the project's cloud policy allows this; the container confines it"
                    }),
                });
            }
        }
        Ok(ApprovalOutcome::Deferred)
    }
}

/// Open the local-tools session over the workspace, at the container level.
///
/// # Errors
///
/// The workspace cannot be opened.
pub fn open_session(config: &RunnerConfig) -> Result<Arc<LocalSession>, String> {
    let prompt = Arc::new(CloudPrompt::default());
    let mut shell = ShellConfig::new(config.data_dir.join("tmp"));
    shell.sandbox.container = true;
    shell.sandbox.container_env =
        elitea_local_tools::sandbox::ContainerEnv::new(config.env.clone());
    let session = LocalSession::open(SessionConfig {
        root: config.root.clone(),
        session_id: "cloud".to_owned(),
        policy: LocalWorkPolicy {
            allowed: true,
            shell: config.policy.shell,
            max_sandbox_mode: SandboxMode::FullAccess,
            network: config.policy.network,
            command_allow: config.policy.command_allow.clone(),
            command_deny: config.policy.command_deny.clone(),
            path_deny: Vec::new(),
            local_index: false,
        },
        settings: WorkspaceSettings::default(),
        choices: Arc::new(MemoryChoices::default()),
        prompt: Arc::clone(&prompt) as Arc<dyn ApprovalChannel>,
        data_dir: config.data_dir.clone(),
        shell: Some(shell),
        deny_read: Vec::new(),
        app_id: None,
    })
    .map_err(|error| error.to_string())?;
    prompt.bind(Arc::clone(session.rules()));
    Ok(session)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    tool: String,
    call_id: String,
    #[serde(default)]
    args: Value,
    /// The worker's HITL approval of this exact call.
    #[serde(default)]
    approved: bool,
}

/// `{"classify": {"command": "...", "cwd": "..."}}`: would this command be
/// asked about? The worker's pre-dispatch question (ADR-0034 decision 9).
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Classify {
    classify: ClassifyArgs,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ClassifyArgs {
    command: String,
    #[serde(default)]
    cwd: Option<String>,
}

/// Whether a person must approve `command` in the cloud: the ADR-0029
/// addendum item 3 list (recursive deletes, history rewrites and forced git,
/// pushes and publishes, privilege, writes outside the workspace), as
/// `elitea_local_tools::classify` reads it. Network use and full access are
/// not asked about: the project setting decided the first, the container
/// confines the second.
#[must_use]
pub fn classify_command(root: &Path, command: &str, cwd: Option<&str>) -> Value {
    use elitea_local_tools::classify::{Place, Risk, assess};
    let cwd = cwd
        .filter(|cwd| {
            Path::new(cwd)
                .components()
                .all(|component| matches!(component, std::path::Component::Normal(_)))
        })
        .map_or_else(|| root.to_path_buf(), |cwd| root.join(cwd));
    match assess(command, Place { root, cwd: &cwd }) {
        Risk::Routine => json!({"ask": false}),
        Risk::Destructive(reason) => json!({"ask": true, "reason": reason}),
    }
}

/// Answer one request line.
pub async fn handle_line(session: &Arc<LocalSession>, line: &str) -> Value {
    if let Ok(Classify { classify }) = serde_json::from_str::<Classify>(line) {
        return classify_command(
            session.workspace().root(),
            &classify.command,
            classify.cwd.as_deref(),
        );
    }
    let request: Request = match serde_json::from_str(line) {
        Ok(request) => request,
        Err(_) => {
            return json!({"status": "error", "code": "runner.bad_request", "message": "the request is not a tool call"});
        }
    };
    if !WORKSPACE_TOOLS.iter().any(|spec| spec.name == request.tool) {
        return json!({
            "status": "error",
            "code": "denied",
            "message": format!("`{}` is not available in the cloud workspace", request.tool),
        });
    }
    if request.approved
        && let Ok(mut approved) = APPROVED.lock()
    {
        approved.push(request.call_id.clone());
    }
    let answer = session
        .call(&request.tool, &request.call_id, request.args)
        .await;
    if let Ok(mut approved) = APPROVED.lock() {
        approved.retain(|seen| *seen != request.call_id);
    }
    answer
}

/// The tools that can grow the workspace.
const MUTATING_TOOLS: &[&str] = &["run_command", "write_file", "edit_file", "apply_patch"];
/// The tools refused while the workspace is over its cap: `run_command`
/// stays, so the agent can remove what it made.
const GROWING_TOOLS: &[&str] = &["write_file", "edit_file", "apply_patch"];

/// The bytes the workspace holds on its volume (every file, `.git`
/// included: it is on the same volume), symlinks not followed.
#[must_use]
pub fn workspace_usage(root: &Path) -> u64 {
    let mut total: u64 = 0;
    let mut stack = vec![root.to_path_buf()];
    while let Some(directory) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&directory) else {
            continue;
        };
        for entry in entries.flatten() {
            let Ok(metadata) = std::fs::symlink_metadata(entry.path()) else {
                continue;
            };
            if metadata.is_dir() {
                stack.push(entry.path());
            } else {
                total = total.saturating_add(metadata.len());
            }
        }
    }
    total
}

/// The workspace cap of one run (ADR-0034 decision 5: the declared
/// unpacked size plus build headroom). On Docker the named volume cannot be
/// sized by the local driver, so the runner enforces it: after every tool
/// that can grow the tree, and before `pack`.
#[derive(Clone, Debug)]
pub struct WorkspaceCap {
    pub root: PathBuf,
    pub max_bytes: u64,
}

impl WorkspaceCap {
    /// `Err((used, max))` when over.
    ///
    /// # Errors
    ///
    /// The workspace holds more than the cap.
    pub fn check(&self) -> Result<u64, (u64, u64)> {
        let used = workspace_usage(&self.root);
        if used > self.max_bytes {
            Err((used, self.max_bytes))
        } else {
            Ok(used)
        }
    }
}

fn over_cap_notice(used: u64, max: u64) -> String {
    format!(
        "The workspace holds {used} bytes, over its cap of {max} bytes. Remove files (for example build outputs) before the turn ends: an oversized workspace is not saved."
    )
}

/// Answer one request line under the workspace cap.
pub async fn handle_line_capped(
    session: &Arc<LocalSession>,
    cap: Option<&WorkspaceCap>,
    line: &str,
) -> Value {
    let tool = serde_json::from_str::<Value>(line)
        .ok()
        .and_then(|value| value.get("tool").and_then(Value::as_str).map(str::to_owned));
    let Some(cap) = cap else {
        return handle_line(session, line).await;
    };
    let tool = tool.unwrap_or_default();
    if GROWING_TOOLS.contains(&tool.as_str())
        && let Err((used, max)) = cap.check()
    {
        return json!({
            "status": "error",
            "code": "workspace_too_large",
            "message": over_cap_notice(used, max),
        });
    }
    let mut answer = handle_line(session, line).await;
    if MUTATING_TOOLS.contains(&tool.as_str())
        && let Err((used, max)) = cap.check()
        && let Some(object) = answer.as_object_mut()
    {
        object.insert(
            "workspace_over_cap".to_owned(),
            json!({"bytes": used, "cap": max}),
        );
        object.insert(
            "notice".to_owned(),
            Value::String(over_cap_notice(used, max)),
        );
    }
    answer
}

/// Serve tool calls until stdin closes.
///
/// # Errors
///
/// Reading or writing the streams failed.
pub async fn serve<R, W>(session: Arc<LocalSession>, input: R, output: W) -> std::io::Result<()>
where
    R: AsyncBufRead + Unpin,
    W: AsyncWrite + Unpin,
{
    serve_capped(session, None, input, output).await
}

/// [`serve`] under a workspace cap.
///
/// # Errors
///
/// Reading or writing the streams failed.
pub async fn serve_capped<R, W>(
    session: Arc<LocalSession>,
    cap: Option<WorkspaceCap>,
    input: R,
    mut output: W,
) -> std::io::Result<()>
where
    R: AsyncBufRead + Unpin,
    W: AsyncWrite + Unpin,
{
    // One turn per runner: its first change takes the one checkpoint.
    session.begin_turn("cloud turn");
    let mut lines = input.lines();
    while let Some(line) = lines.next_line().await? {
        let answer = if line.len() > MAX_REQUEST_LINE_BYTES {
            json!({"status": "error", "code": "runner.request_too_large", "message": "the request is too large"})
        } else {
            handle_line_capped(&session, cap.as_ref(), &line).await
        };
        let mut encoded = serde_json::to_vec(&answer).unwrap_or_else(|_| b"{}".to_vec());
        encoded.push(b'\n');
        output.write_all(&encoded).await?;
        output.flush().await?;
    }
    Ok(())
}

/// Read the archive on `input` into the (empty) workspace and record it as
/// the baseline commit the git tools compare against.
///
/// # Errors
///
/// A refused archive, or git failing.
pub fn unpack(
    input: &mut dyn std::io::Read,
    root: &Path,
    limits: ArchiveLimits,
) -> Result<ArchiveStats, String> {
    let stats = workspace_archive::read_tree(input, root, limits)
        .map_err(|error: ArchiveError| error.code().to_owned())?;
    git_baseline(root)?;
    Ok(stats)
}

/// [`pack`], refused (`runner.workspace_too_large`) when the workspace is
/// over its cap.
///
/// # Errors
///
/// Over the cap, or as [`pack`].
pub fn pack_capped(
    root: &Path,
    output: &mut dyn std::io::Write,
    limits: ArchiveLimits,
    cap: Option<&WorkspaceCap>,
) -> Result<ArchiveStats, String> {
    if let Some(cap) = cap
        && cap.check().is_err()
    {
        return Err("runner.workspace_too_large".to_owned());
    }
    pack(root, output, limits)
}

/// Write the workspace, without `.git`, to `output`.
///
/// # Errors
///
/// A tree that cannot be moved.
pub fn pack(
    root: &Path,
    output: &mut dyn std::io::Write,
    limits: ArchiveLimits,
) -> Result<ArchiveStats, String> {
    workspace_archive::write_tree(root, output, limits).map_err(|error| error.code().to_owned())
}

/// `git init` and one commit of the checkout, with no hook, no global or
/// system config, and no signing. Inside the container only.
fn git_baseline(root: &Path) -> Result<(), String> {
    let git = |args: &[&str]| -> Result<(), String> {
        let status = std::process::Command::new("git")
            .args([
                "-c",
                "core.hooksPath=/dev/null",
                "-c",
                "core.fsmonitor=false",
                "-c",
                "commit.gpgSign=false",
                "-c",
                "user.name=Elitea cloud executor",
                "-c",
                "user.email=cloud-executor@elitea.invalid",
            ])
            .args(args)
            .current_dir(root)
            .env("GIT_CONFIG_NOSYSTEM", "1")
            .env("GIT_CONFIG_GLOBAL", "/dev/null")
            .env("GIT_TERMINAL_PROMPT", "0")
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
            .map_err(|_| "git is not available in the runner image".to_owned())?;
        if status.success() {
            Ok(())
        } else {
            Err(format!(
                "git {} failed",
                args.first().copied().unwrap_or("")
            ))
        }
    };
    git(&["init", "-q", "-b", "main"])?;
    git(&["add", "-A"])?;
    git(&[
        "commit",
        "-q",
        "--allow-empty",
        "--no-verify",
        "-m",
        "Elitea cloud checkout",
    ])
}
