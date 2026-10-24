//! The person's hooks (#1237): commands from host data that run around the
//! local tools, sandboxed, with the call as JSON on stdin.
//!
//! * [`config`]: the `hooks.json` files in host data (never the workspace),
//!   schema-checked, owner-only.
//! * [`proposal`]: a repository's `.elitea/hooks.json` (and the hooks of
//!   `.claude/settings.json`) as a proposal the person trusts hook by hook;
//!   nothing runs from it.
//! * [`HookSet`]: one session's hooks and their runner.
//!
//! **Running.** Hooks of an event run one after another in the files'
//! order, the person's file first. Each is its own process group under
//! [`crate::sandbox::prepare`] with the session's deny list and `path_deny`,
//! in its sandbox capped by the policy's `max_sandbox_mode`, the network on
//! only when the policy and the hook both allow it, in the workspace root,
//! with an environment built from nothing (`PATH` without relative
//! entries, `HOME`, the locale, `TMPDIR`, `TERM=dumb`, a few toolchain
//! homes): never the host's. A sandbox that cannot hide what the session
//! denies does not run it (a hook is never asked about). Its timeout (at
//! most 30 s, and at most 30 s for all of one call's hooks) kills the whole
//! group; stdout is kept up to 64 KiB, stderr up to 16 KiB. No hook runs
//! when the policy turns local work or the shell off.
//!
//! **Stdin**: `{"event", "session_id", "conversation_uuid", "tool",
//! "input", "cwd": ".", "workspace_name"}`, with `"output"` (cut at 64 KiB)
//! for `PostToolUse` and `"prompt"` for `UserPromptSubmit`: never the
//! workspace's absolute path. **Stdout** (exit 0, optional): `{"decision":
//! "allow"|"deny"|"ask", "reason", "modified_input", "annotation"}`; text
//! that is not JSON is an annotation. Exit 2 denies with stderr as the
//! reason. Any other exit, a timeout or a hook that cannot start is a
//! failure: reported, and the call proceeds, unless the hook is
//! `fail_closed`.
//!
//! **What a verdict can do** (the session applies it, [`crate::session`]):
//! a `PreToolUse` deny refuses the call with a `hook_denied` error the model
//! reads; `ask` forces the approval prompt; `allow` changes nothing (hooks
//! tighten, never loosen); `modified_input` is checked against the tool's
//! schema and the call is then decided by the whole rule stack on it. A
//! `PostToolUse` annotation is added to the result. Everything a hook
//! writes reaches the model framed as untrusted text ([`HookFramer`]).
//!
//! Local-only: the cloud executor runs no hook.

pub mod config;
pub mod proposal;

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use globset::{Glob, GlobSetBuilder};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::{AsyncRead, AsyncReadExt as _, AsyncWriteExt as _};

use crate::policy::{LocalWorkPolicy, SandboxMode};
use crate::shell::{self, ShellConfig};
use crate::workspace::Workspace;
pub use config::{Hook, HookEvent, HookMatch, HooksConfig, Scope};

/// Bytes of a hook's stdout kept.
pub const MAX_STDOUT: usize = 64 * 1024;

/// Bytes of a hook's stderr kept.
pub const MAX_STDERR: usize = 16 * 1024;

/// Bytes of a tool's output a `PostToolUse` hook receives.
pub const MAX_OUTPUT_IN: usize = 64 * 1024;

/// Bytes of a tool's input a hook receives whole; a larger input is cut
/// (and a modification from that hook is refused).
pub const MAX_INPUT_IN: usize = 1024 * 1024;

/// What all of one call's (or one turn end's) hooks may take together.
pub const TOTAL_BUDGET: Duration = Duration::from_millis(config::MAX_TIMEOUT_MS);

/// Bytes of a hook's reason or annotation kept.
pub const MAX_TEXT: usize = 8 * 1024;

/// How long the readers may drain after the group is killed.
const DRAIN_AFTER_KILL: Duration = Duration::from_secs(2);

/// How a host frames a hook's text for its model (the desktop's per-thread
/// nonce frame): untrusted data, never instructions.
pub trait HookFramer: Send + Sync {
    /// `text` that the hook `hook` wrote, framed.
    fn frame(&self, hook: &str, text: &str) -> String;
}

/// What became of one hook run.
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RunResult {
    /// It ran and had nothing to say, or allowed.
    Ok,
    Deny,
    Ask,
    /// It changed the call's input.
    Modified,
    /// It added text (a result annotation, the turn's context, a note).
    Annotated,
    Failed,
    TimedOut,
}

/// One hook run, as the settings pane and the transcript show it.
#[derive(Clone, Debug, Serialize)]
pub struct HookRun {
    pub id: String,
    pub event: HookEvent,
    pub result: RunResult,
    pub duration_ms: u64,
    /// When it ended, in milliseconds since the Unix epoch.
    pub at_ms: u64,
    /// For the person (never the model): why it failed, or what it said.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

impl HookRun {
    /// The run as a tool result records it: only what the host wrote.
    #[must_use]
    pub fn record(&self) -> Value {
        json!({ "id": self.id, "result": self.result, "duration_ms": self.duration_ms })
    }
}

/// A hook's refusal.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Denial {
    pub hook: String,
    /// What the hook wrote (untrusted), or the host's reason for a
    /// fail-closed failure.
    pub reason: String,
    /// The reason is the host's (a fail-closed failure), not the hook's.
    pub by_host: bool,
}

/// What an event's hooks decided.
#[derive(Clone, Debug, Default)]
pub struct HookOutcome {
    pub runs: Vec<HookRun>,
    pub denied: Option<Denial>,
    /// The first hook that asked, and its reason (untrusted).
    pub ask: Option<(String, String)>,
    /// The input after every modification (`PreToolUse`).
    pub modified_input: Option<Value>,
    /// Each hook's text (untrusted), in order.
    pub annotations: Vec<(String, String)>,
}

impl HookOutcome {
    /// The runs as a tool result records them.
    #[must_use]
    pub fn records(&self) -> Value {
        Value::Array(self.runs.iter().map(HookRun::record).collect())
    }
}

/// What the runner needs of a session.
pub struct HookEnv<'a> {
    pub workspace: &'a Workspace,
    pub shell: &'a ShellConfig,
    pub policy: &'a LocalWorkPolicy,
    pub session_id: &'a str,
    pub conversation: Option<String>,
}

/// One event to run hooks for.
#[derive(Clone, Debug)]
pub struct HookCall<'a> {
    pub event: HookEvent,
    pub tool: Option<&'a str>,
    pub input: Option<Value>,
    pub output: Option<&'a Value>,
    /// The workspace-relative paths the call touches.
    pub paths: Vec<String>,
    pub prompt: Option<&'a str>,
}

impl HookCall<'_> {
    #[must_use]
    pub fn event(event: HookEvent) -> Self {
        Self {
            event,
            tool: None,
            input: None,
            output: None,
            paths: Vec::new(),
            prompt: None,
        }
    }
}

/// One session's hooks: configured by the host from both files (each turn
/// takes them as they are then), run by the session.
#[derive(Default)]
pub struct HookSet {
    hooks: RwLock<Arc<Vec<(Scope, Hook)>>>,
    framer: RwLock<Option<Arc<dyn HookFramer>>>,
    last: Mutex<BTreeMap<String, HookRun>>,
    unconfined: AtomicBool,
}

/// The reply a hook may print.
#[derive(Deserialize)]
struct Reply {
    #[serde(default)]
    decision: Option<String>,
    #[serde(default)]
    reason: Option<String>,
    #[serde(default)]
    modified_input: Option<Value>,
    #[serde(default)]
    annotation: Option<String>,
}

/// What one hook process did.
struct Raw {
    exit: Option<i32>,
    timed_out: bool,
    stdout: Vec<u8>,
    stdout_over: bool,
    stderr: Vec<u8>,
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |elapsed| {
            u64::try_from(elapsed.as_millis()).unwrap_or(u64::MAX)
        })
}

fn clip(text: &str) -> String {
    elitea_agent_runtime::text::clip_bytes(text.trim(), MAX_TEXT).to_owned()
}

impl HookSet {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Take the hooks to run from now on (both files merged,
    /// [`config::merged`]).
    pub fn configure(&self, hooks: Vec<(Scope, Hook)>) {
        if let Ok(mut current) = self.hooks.write() {
            *current = Arc::new(hooks);
        }
    }

    /// The hooks configured now.
    #[must_use]
    pub fn hooks(&self) -> Arc<Vec<(Scope, Hook)>> {
        self.hooks
            .read()
            .map(|hooks| Arc::clone(&hooks))
            .unwrap_or_default()
    }

    /// Whether any hook runs for `event`.
    #[must_use]
    pub fn has(&self, event: HookEvent) -> bool {
        self.hooks().iter().any(|(_, hook)| hook.event == event)
    }

    /// How hook text is framed for the model from now on (the host sets it
    /// per turn, with the thread's nonce). Without one, no hook text
    /// reaches the model: a refusal names only the hook.
    pub fn set_framer(&self, framer: Option<Arc<dyn HookFramer>>) {
        if let Ok(mut current) = self.framer.write() {
            *current = framer;
        }
    }

    /// `text` from `hook` framed for the model, if a framer is set.
    #[must_use]
    pub fn frame(&self, hook: &str, text: &str) -> Option<String> {
        let framer = self.framer.read().ok()?.clone()?;
        Some(framer.frame(hook, text))
    }

    /// Run hooks where the sandbox cannot hide what the session denies
    /// (tests, and hosts with no OS sandbox that accept it). The desktop
    /// never sets it: there such a hook does not run.
    pub fn allow_unconfined(&self, allowed: bool) {
        self.unconfined.store(allowed, Ordering::Relaxed);
    }

    /// The last run of each hook (the settings pane's status).
    #[must_use]
    pub fn last_runs(&self) -> Vec<HookRun> {
        self.last
            .lock()
            .map(|last| last.values().cloned().collect())
            .unwrap_or_default()
    }

    fn remember(&self, run: &HookRun) {
        if let Ok(mut last) = self.last.lock() {
            last.insert(run.id.clone(), run.clone());
        }
    }

    /// Run `call`'s event's hooks that match it, in order, within
    /// [`TOTAL_BUDGET`]. A deny stops the rest.
    pub async fn run(&self, env: &HookEnv<'_>, call: HookCall<'_>) -> HookOutcome {
        let mut outcome = HookOutcome::default();
        if !(env.policy.allowed && env.policy.shell) {
            return outcome;
        }
        let hooks = self.hooks();
        let mut input = call.input.clone();
        let started = Instant::now();
        for (_, hook) in hooks.iter().filter(|(_, hook)| hook.event == call.event) {
            let Some(paths) = matching_paths(hook, &call) else {
                continue;
            };
            let begun = Instant::now();
            let remaining = TOTAL_BUDGET.saturating_sub(started.elapsed());
            let budget = remaining.min(Duration::from_millis(hook.timeout_ms));
            let (stdin, input_cut) = stdin_json(env, &call, input.as_ref());
            let raw = if budget.is_zero() {
                Err("the 30 s every hook of one call shares was used up".to_owned())
            } else {
                self.spawn(env, hook, &call, &paths, stdin, budget).await
            };
            let mut run = HookRun {
                id: hook.id.clone(),
                event: hook.event,
                result: RunResult::Ok,
                duration_ms: u64::try_from(begun.elapsed().as_millis()).unwrap_or(u64::MAX),
                at_ms: now_ms(),
                message: None,
            };
            let failure = match raw {
                Err(why) => Some((RunResult::Failed, why)),
                Ok(raw) if raw.timed_out => Some((
                    RunResult::TimedOut,
                    format!(
                        "it ran longer than {} ms and was stopped",
                        budget.as_millis()
                    ),
                )),
                Ok(raw) => match interpret(hook, &raw, input_cut, &mut outcome, &mut input) {
                    Ok(result) => {
                        run.result = result;
                        None
                    }
                    Err(why) => Some((RunResult::Failed, why)),
                },
            };
            if let Some((result, why)) = failure {
                tracing::warn!(hook = %hook.id, event = hook.event.as_str(), reason = %why, "a hook failed");
                run.result = result;
                run.message = Some(why.clone());
                let closes = hook.fail_closed
                    && matches!(
                        hook.event,
                        HookEvent::PreToolUse | HookEvent::UserPromptSubmit
                    );
                if closes {
                    outcome.denied = Some(Denial {
                        hook: hook.id.clone(),
                        reason: format!("the hook failed ({why}) and is fail-closed"),
                        by_host: true,
                    });
                }
            } else if let Some(denial) = &outcome.denied {
                run.message = Some(clip(&denial.reason));
            } else if let Some((_, text)) =
                outcome.annotations.last().filter(|(id, _)| id == &hook.id)
            {
                run.message = Some(clip(text));
            }
            self.remember(&run);
            outcome.runs.push(run);
            if outcome.denied.is_some() {
                break;
            }
        }
        if input != call.input {
            outcome.modified_input = input;
        }
        outcome
    }

    /// Start `hook` sandboxed, feed it `stdin`, and collect what it did
    /// within `timeout`; `Err` when it could not start.
    async fn spawn(
        &self,
        env: &HookEnv<'_>,
        hook: &Hook,
        call: &HookCall<'_>,
        paths: &[String],
        stdin: Vec<u8>,
        timeout: Duration,
    ) -> Result<Raw, String> {
        let argv = self.argv(env, hook, call, paths)?;
        let mode = hook
            .sandbox
            .min(SandboxMode::WorkspaceWrite)
            .min(env.policy.max_sandbox_mode);
        let network = hook.network && env.policy.network;
        let temp = hook_temp(env.shell)?;
        let request = shell::sandbox_request(env.workspace, env.shell, mode, network)
            .map_err(|error| error.message().to_owned())?;
        let prepared = crate::sandbox::prepare(&request, &argv, &env.shell.sandbox)
            .map_err(|error| error.message().to_owned())?;
        if !prepared.hides_denied && !self.unconfined.load(Ordering::Relaxed) {
            return Err(format!(
                "the sandbox cannot hide what this session may not read ({}), so the hook did \
                 not run",
                prepared.note.unwrap_or("no sandbox")
            ));
        }
        let mut command = tokio::process::Command::new(&prepared.program);
        command
            .args(&prepared.args)
            .current_dir(env.workspace.root())
            .env_clear()
            .envs(environment(env.shell, &temp, hook))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .process_group(0)
            .kill_on_drop(true);
        let mut child = command
            .spawn()
            .map_err(|error| format!("it could not start: {}", error.kind()))?;
        let mut group = GroupKill(
            child
                .id()
                .and_then(|id| i32::try_from(id).ok())
                .and_then(rustix::process::Pid::from_raw),
        );
        let (Some(mut pipe_in), Some(out), Some(err)) =
            (child.stdin.take(), child.stdout.take(), child.stderr.take())
        else {
            return Err("it has no pipes".to_owned());
        };
        // Written beside the reads: a hook that prints before it reads its
        // stdin must not block on a full pipe. A hook that never reads gets
        // EPIPE, which is not its failure.
        let writer = tokio::spawn(async move {
            let _ = pipe_in.write_all(&stdin).await;
            let _ = pipe_in.shutdown().await;
        });
        let mut out_reader = tokio::spawn(bounded(out, MAX_STDOUT));
        let mut err_reader = tokio::spawn(bounded(err, MAX_STDERR));
        let waited = tokio::time::timeout(timeout, child.wait()).await;
        let (status, timed_out) = if let Ok(status) = waited {
            (status.ok(), false)
        } else {
            group.kill_now();
            let _ = child.start_kill();
            (child.wait().await.ok(), true)
        };
        // What it left in its group goes too, then a bounded drain.
        group.kill_now();
        writer.abort();
        let drained = tokio::time::timeout(DRAIN_AFTER_KILL, async {
            let out = (&mut out_reader).await.unwrap_or_default();
            let err = (&mut err_reader).await.unwrap_or_default();
            (out, err)
        })
        .await;
        out_reader.abort();
        err_reader.abort();
        if mode != SandboxMode::ReadOnly {
            env.shell.sandbox.masks.invalidate();
        }
        let ((stdout, stdout_over), (stderr, _)) = drained.unwrap_or_default();
        Ok(Raw {
            exit: status
                .filter(|_| !timed_out)
                .and_then(|status| status.code()),
            timed_out,
            stdout,
            stdout_over,
            stderr,
        })
    }

    /// The hook's argv: its program found (outside the workspace and the
    /// denied folders) and its placeholders filled in.
    fn argv(
        &self,
        env: &HookEnv<'_>,
        hook: &Hook,
        call: &HookCall<'_>,
        paths: &[String],
    ) -> Result<Vec<String>, String> {
        let _ = self;
        let program = find_program(&hook.command[0])
            .ok_or_else(|| format!("`{}` is not an executable program", hook.command[0]))?;
        let resolved = std::fs::canonicalize(&program).unwrap_or_else(|_| program.clone());
        if resolved.starts_with(env.workspace.root()) || program.starts_with(env.workspace.root()) {
            return Err("a hook's program cannot be inside the workspace".to_owned());
        }
        let deny = env.shell.deny_list();
        if deny.covers(&resolved) || deny.covers(&program) {
            return Err("a hook's program cannot be in a denied folder".to_owned());
        }
        let mut argv = vec![program.display().to_string()];
        for item in &hook.command[1..] {
            match item.as_str() {
                "{path}" => argv.extend(paths.iter().map(|path| {
                    // A path that reads as an option is not one.
                    if path.starts_with('-') {
                        format!("./{path}")
                    } else {
                        path.clone()
                    }
                })),
                "{tool}" => argv.extend(call.tool.map(str::to_owned)),
                _ => argv.push(item.clone()),
            }
        }
        Ok(argv)
    }
}

/// What exit 2 means: a refusal before a call or a message, text after one.
fn exit_two(hook: &Hook, stderr: &str, outcome: &mut HookOutcome) -> RunResult {
    let reason = if stderr.trim().is_empty() {
        "no reason given".to_owned()
    } else {
        clip(stderr)
    };
    match hook.event {
        HookEvent::PreToolUse | HookEvent::UserPromptSubmit => {
            outcome.denied = Some(Denial {
                hook: hook.id.clone(),
                reason,
                by_host: false,
            });
            RunResult::Deny
        }
        HookEvent::PostToolUse | HookEvent::Stop => {
            outcome.annotations.push((hook.id.clone(), reason));
            RunResult::Annotated
        }
    }
}

/// One hook's exit and output as a verdict (recorded in `outcome`, the
/// input it modified in `input`), or why it is a failure.
fn interpret(
    hook: &Hook,
    raw: &Raw,
    input_cut: bool,
    outcome: &mut HookOutcome,
    input: &mut Option<Value>,
) -> Result<RunResult, String> {
    let stderr = String::from_utf8_lossy(&raw.stderr);
    match raw.exit {
        Some(2) => return Ok(exit_two(hook, &stderr, outcome)),
        Some(0) => {}
        Some(code) => {
            let detail = clip(&stderr);
            return Err(if detail.is_empty() {
                format!("it exited with {code}")
            } else {
                format!("it exited with {code}: {detail}")
            });
        }
        None => return Err("it was killed by a signal".to_owned()),
    }
    if raw.stdout_over {
        return Err(format!(
            "it wrote more than {} KiB to stdout",
            MAX_STDOUT / 1024
        ));
    }
    let stdout = String::from_utf8_lossy(&raw.stdout);
    let text = stdout.trim();
    if text.is_empty() {
        return Ok(RunResult::Ok);
    }
    let Ok(reply) = serde_json::from_str::<Reply>(text) else {
        // Not a reply: text, which annotates where an event takes text.
        if hook.event == HookEvent::PreToolUse {
            return Ok(RunResult::Ok);
        }
        outcome.annotations.push((hook.id.clone(), clip(text)));
        return Ok(RunResult::Annotated);
    };
    let reason = reply.reason.as_deref().map(clip).unwrap_or_default();
    let pre = matches!(
        hook.event,
        HookEvent::PreToolUse | HookEvent::UserPromptSubmit
    );
    match reply.decision.as_deref() {
        Some("deny") if pre => {
            outcome.denied = Some(Denial {
                hook: hook.id.clone(),
                reason: if reason.is_empty() {
                    "no reason given".to_owned()
                } else {
                    reason
                },
                by_host: false,
            });
            return Ok(RunResult::Deny);
        }
        Some("ask") if hook.event == HookEvent::PreToolUse => {
            if outcome.ask.is_none() {
                outcome.ask = Some((hook.id.clone(), reason));
            }
            return Ok(RunResult::Ask);
        }
        // An allow changes nothing (hooks only tighten), nor does a
        // verdict the event does not take.
        None | Some("allow" | "deny" | "ask") => {}
        Some(other) => return Err(format!("`{other}` is not a decision")),
    }
    let mut result = RunResult::Ok;
    if let Some(modified) = reply.modified_input {
        if hook.event != HookEvent::PreToolUse {
            return Err("only a PreToolUse hook may modify the input".to_owned());
        }
        if input_cut {
            return Err(format!(
                "it modified an input it saw cut at {} KiB",
                MAX_INPUT_IN / 1024
            ));
        }
        if !modified.is_object() {
            return Err("`modified_input` is not an object".to_owned());
        }
        *input = Some(modified);
        result = RunResult::Modified;
    }
    if let Some(text) = reply.annotation.filter(|text| !text.trim().is_empty())
        && hook.event != HookEvent::PreToolUse
    {
        outcome.annotations.push((hook.id.clone(), clip(&text)));
        if result == RunResult::Ok {
            result = RunResult::Annotated;
        }
    }
    Ok(result)
}

/// The paths of `call` a tool hook matches (all of them without `paths`
/// globs), or `None` when it does not run for this call.
fn matching_paths(hook: &Hook, call: &HookCall<'_>) -> Option<Vec<String>> {
    if !hook.event.is_tool_event() {
        return Some(Vec::new());
    }
    let tool = call.tool?;
    if !hook.matcher.names_tool(tool) {
        return None;
    }
    if hook.matcher.paths.is_empty() {
        return Some(call.paths.clone());
    }
    let mut builder = GlobSetBuilder::new();
    for glob in &hook.matcher.paths {
        builder.add(Glob::new(glob).ok()?);
    }
    let set = builder.build().ok()?;
    let matched: Vec<String> = call
        .paths
        .iter()
        .filter(|path| set.is_match(path.as_str()))
        .cloned()
        .collect();
    (!matched.is_empty()).then_some(matched)
}

/// The JSON a hook reads, and whether the input in it was cut.
fn stdin_json(env: &HookEnv<'_>, call: &HookCall<'_>, input: Option<&Value>) -> (Vec<u8>, bool) {
    let workspace_name = env
        .workspace
        .root()
        .file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .unwrap_or_default();
    let mut body = json!({
        "event": call.event,
        "session_id": env.session_id,
        "conversation_uuid": env.conversation,
        "cwd": ".",
        "workspace_name": workspace_name,
    });
    let mut cut = false;
    if let Some(tool) = call.tool {
        body["tool"] = json!(tool);
    }
    if let Some(input) = input {
        let text = input.to_string();
        body["input"] = if text.len() > MAX_INPUT_IN {
            cut = true;
            json!({
                "truncated": true,
                "text": elitea_agent_runtime::text::clip_bytes(&text, MAX_INPUT_IN),
            })
        } else {
            input.clone()
        };
    }
    if let Some(output) = call.output {
        let text = output.to_string();
        body["output"] = if text.len() > MAX_OUTPUT_IN {
            json!({
                "truncated": true,
                "text": elitea_agent_runtime::text::clip_bytes(&text, MAX_OUTPUT_IN),
            })
        } else {
            output.clone()
        };
    }
    if let Some(prompt) = call.prompt {
        body["prompt"] = json!(prompt);
    }
    let mut bytes = body.to_string().into_bytes();
    bytes.push(b'\n');
    (bytes, cut)
}

/// `name` as the hook runs it: an absolute path that is an executable
/// file, or the first executable of that name on the absolute `PATH`
/// entries (the path as found, not resolved: a launcher such as rustup's
/// `cargo` reads its own name).
fn find_program(name: &str) -> Option<PathBuf> {
    use std::os::unix::fs::PermissionsExt as _;
    let executable = |path: &Path| {
        path.metadata()
            .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
    };
    if name.contains('/') {
        let path = PathBuf::from(name);
        return (path.is_absolute() && executable(&path)).then_some(path);
    }
    crate::command::search_path()
        .into_iter()
        .map(|dir| dir.join(name))
        .find(|path| executable(path))
}

/// The hooks' temporary folder (`TMPDIR`), `0700` under the session's.
fn hook_temp(shell: &ShellConfig) -> Result<PathBuf, String> {
    use std::os::unix::fs::DirBuilderExt as _;
    let temp = shell.temp_dir.join("hooks");
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(&temp)
        .map_err(|error| format!("its temporary folder cannot be created: {}", error.kind()))?;
    std::fs::canonicalize(&temp)
        .map_err(|error| format!("its temporary folder cannot be resolved: {}", error.kind()))
}

/// Host variables a hook may see, by name (values are paths and locale
/// settings, never credentials).
const ALLOWED_ENV: &[&str] = &[
    "USER",
    "LOGNAME",
    "LANG",
    "LC_ALL",
    "LC_CTYPE",
    "CARGO_HOME",
    "RUSTUP_HOME",
    "GOPATH",
    "GOROOT",
];

/// A hook's whole environment, built from nothing (the host's own git
/// does the same): `PATH` without relative entries, `HOME`, the allowed
/// names, `TMPDIR` and the hook's own markers.
fn environment(shell: &ShellConfig, temp: &Path, hook: &Hook) -> Vec<(String, String)> {
    let mut env: Vec<(String, String)> = ALLOWED_ENV
        .iter()
        .filter_map(|name| {
            std::env::var(name)
                .ok()
                .map(|value| ((*name).to_owned(), value))
        })
        .collect();
    if let Ok(path) = std::env::join_paths(crate::command::search_path()) {
        env.push(("PATH".to_owned(), path.to_string_lossy().into_owned()));
    }
    if let Some(home) = shell.home_dir() {
        env.push(("HOME".to_owned(), home.display().to_string()));
    }
    env.extend(
        [
            ("TMPDIR", temp.display().to_string()),
            ("TERM", "dumb".to_owned()),
            ("NO_COLOR", "1".to_owned()),
            ("GIT_TERMINAL_PROMPT", "0".to_owned()),
            ("ELITEA_HOOK_ID", hook.id.clone()),
            ("ELITEA_HOOK_EVENT", hook.event.as_str().to_owned()),
        ]
        .map(|(name, value)| (name.to_owned(), value)),
    );
    env
}

/// Read up to `cap` bytes, then keep draining (so the writer never blocks)
/// and note that more came.
async fn bounded<R: AsyncRead + Unpin>(mut reader: R, cap: usize) -> (Vec<u8>, bool) {
    let mut kept = Vec::new();
    let mut over = false;
    let mut buffer = [0u8; 8192];
    while let Ok(read) = reader.read(&mut buffer).await {
        if read == 0 {
            break;
        }
        let room = cap.saturating_sub(kept.len());
        kept.extend_from_slice(&buffer[..read.min(room)]);
        over |= read > room;
    }
    (kept, over)
}

/// Kills a hook's process group when dropped (the caller stopped waiting).
struct GroupKill(Option<rustix::process::Pid>);

impl GroupKill {
    fn kill_now(&mut self) {
        if let Some(group) = self.0.take() {
            let _ = rustix::process::kill_process_group(group, rustix::process::Signal::KILL);
        }
    }
}

impl Drop for GroupKill {
    fn drop(&mut self) {
        self.kill_now();
    }
}

/// The workspace paths a tool call names in its arguments, as the model
/// wrote them: `path`, `cwd` (a command's, `.` by default) and the files of
/// an `apply_patch`.
#[must_use]
pub fn call_paths(tool: &str, args: &Value) -> Vec<String> {
    let mut out = Vec::new();
    if let Some(path) = args.get("path").and_then(Value::as_str) {
        out.push(path.to_owned());
    }
    if matches!(tool, "run_command" | "run_tests") {
        out.push(
            args.get("cwd")
                .and_then(Value::as_str)
                .unwrap_or(".")
                .to_owned(),
        );
    }
    if let Some(patch) = args.get("patch").and_then(Value::as_str) {
        for line in patch.lines() {
            let named = [
                "*** Add File: ",
                "*** Update File: ",
                "*** Delete File: ",
                "*** Move to: ",
            ]
            .iter()
            .find_map(|prefix| line.strip_prefix(prefix))
            .or_else(|| {
                line.strip_prefix("+++ ")
                    .or_else(|| line.strip_prefix("--- "))
                    .map(|name| {
                        let name = name.split('\t').next().unwrap_or(name);
                        name.strip_prefix("a/")
                            .or_else(|| name.strip_prefix("b/"))
                            .unwrap_or(name)
                    })
                    .filter(|name| *name != "/dev/null")
            });
            if let Some(name) = named.map(str::trim).filter(|name| !name.is_empty())
                && !out.iter().any(|known| known == name)
            {
                out.push(name.to_owned());
            }
        }
    }
    out
}

/// Check `value` against a tool's parameter schema as far as its top level
/// goes: an object, every `required` key present, no key the schema does
/// not name, each named key of its `type` (and within its `enum`, `minimum`
/// and `maximum`). What a hook's `modified_input` must pass before the
/// tool's own argument parsing and the rules see it.
///
/// # Errors
///
/// Why it does not fit.
pub fn check_schema(schema: &Value, value: &Value) -> Result<(), String> {
    let object = value
        .as_object()
        .ok_or_else(|| "the input is not an object".to_owned())?;
    let properties = schema
        .get("properties")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default();
    for required in schema
        .get("required")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
    {
        if !object.contains_key(required) {
            return Err(format!("`{required}` is required"));
        }
    }
    for (key, item) in object {
        let property = properties
            .get(key)
            .ok_or_else(|| format!("`{key}` is not a parameter of this tool"))?;
        let fits = match property.get("type").and_then(Value::as_str) {
            Some("string") => item.is_string(),
            Some("integer") => item.is_i64() || item.is_u64(),
            Some("number") => item.is_number(),
            Some("boolean") => item.is_boolean(),
            Some("array") => item.is_array(),
            Some("object") => item.is_object(),
            _ => true,
        };
        if !fits {
            return Err(format!("`{key}` is not of the parameter's type"));
        }
        if let Some(allowed) = property.get("enum").and_then(Value::as_array)
            && !allowed.contains(item)
        {
            return Err(format!("`{key}` is not one of the allowed values"));
        }
        if let Some(number) = item.as_f64() {
            let below = property
                .get("minimum")
                .and_then(Value::as_f64)
                .is_some_and(|min| number < min);
            let above = property
                .get("maximum")
                .and_then(Value::as_f64)
                .is_some_and(|max| number > max);
            if below || above {
                return Err(format!("`{key}` is out of range"));
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests;
