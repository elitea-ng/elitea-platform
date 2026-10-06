//! The `native` runner: the Rust engine behind the socket (ADR-0026).
//!
//! `generate_wiki` runs in a CHILD process of this binary
//! (`elitea-deepwiki-engine worker`, see [`crate::worker`]), as decision 10
//! requires: the child has its own address-space and CPU limits and its own
//! scratch directory, so one runaway generation cannot take the sidecar (and
//! every other invocation) down with it. This module is the parent's half:
//!
//! 1. a scratch directory per job, `{ELITEA_DEEPWIKI_SCRATCH_PATH}/jobs/…`,
//!    mode 0700, removed when the child has ended;
//! 2. the child is spawned with `RAYON_NUM_THREADS` and `MALLOC_ARENA_MAX`
//!    set (both bound the address space the limit counts) and the request
//!    on its STDIN — the arguments carry credentials (`repo_config`,
//!    `llm_settings`), so never `argv` or the environment. Stdin stays open:
//!    its end tells the child the parent is gone;
//! 3. the child's NDJSON stdout is relayed (`thinking`, `token`); a
//!    `{"build": id}` line is kept, not relayed; the last line is the
//!    result or the error. The child's stderr (its logs) is copied to ours;
//! 4. a stop (or a reader that went away) sends SIGTERM, then SIGKILL after
//!    [`KILL_AFTER`], as the Python sidecar did;
//! 5. after the child has ended, its build — if it reported one — is
//!    deleted, so a killed child leaves no staging rows (a published or
//!    abandoned build is already gone and the delete is a no-op).
//!
//! `ask`, `deep_research` and `resolve_wiki` are refused until their ports
//! land (ADR-0026 phase 6).

use super::{Context, prepare_arguments};
use crate::cgroup::Cgroup;
use crate::config::{ConfigError, Settings};
use crate::errors::{EngineError, ErrorType};
use crate::storage::build::{delete_build, new_boot_id, process_boot_id};
use serde_json::{Map, Value, json};
use sqlx::PgPool;
use std::os::unix::fs::DirBuilderExt;
use std::os::unix::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::process::{ChildStderr, ChildStdout, Command};
use tokio::sync::mpsc;

/// How long a stopped child has between SIGTERM and SIGKILL.
pub const KILL_AFTER: Duration = Duration::from_secs(3);

/// The largest NDJSON line, newline included, that this engine sends: the
/// result line carries every page. It is the Go host's limit — the
/// `bufio.Scanner` buffer of `services/elitea-subapp-host/internal/engine/
/// engine.go` (`scanner.Buffer(…, 64*1024*1024)`) — so a larger line could
/// never reach the host. The two numbers must stay equal; a test reads the
/// Go source. The parent reads no longer line from its child, and the child
/// refuses a result this large BEFORE it publishes
/// (`generate::check_result_size`).
pub const MAX_RESULT_LINE: usize = 64 * 1024 * 1024;

/// The longest stderr line copied (longer ones are cut).
const MAX_LOG_LINE: usize = 64 * 1024;

/// What the Rust runtime prints when an allocation fails (then it aborts).
const ALLOCATION_FAILED: &str = "memory allocation of ";

/// The subcommand the child runs.
pub const WORKER_SUBCOMMAND: &str = "worker";

/// How the child is started.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WorkerCommand {
    /// The engine binary (this process's own executable in production).
    pub program: PathBuf,
    /// Extra environment for the child, on top of this process's (tests
    /// give the child its settings this way).
    pub env: Vec<(String, String)>,
}

/// The native runner.
#[derive(Debug, Clone)]
pub struct NativeRunner {
    settings: Arc<Settings>,
    worker: Arc<WorkerCommand>,
    /// For deleting the build of a child that was killed.
    pool: PgPool,
    /// This process's cgroup (v2), whose OOM-kill count explains a SIGKILL
    /// this runner did not send.
    cgroup: Option<Arc<Cgroup>>,
}

/// What the parent saw of a child that ended without a last line.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
struct ExitFacts {
    /// The child's stderr reported a failed allocation.
    allocation_failed: bool,
    /// This runner sent the child SIGKILL.
    killed_by_parent: bool,
    /// The child's stdout ended inside a line.
    cut_off: Option<String>,
    /// The cgroup's `oom_kill` count when the child started and after it
    /// ended, when readable.
    oom_kills: Option<(u64, u64)>,
}

/// How a child's stdout could not be read.
#[derive(Debug, Clone, PartialEq, Eq)]
enum ReadFailure {
    /// A line too long or not JSON: the child is misbehaving.
    Unreadable(String),
    /// The stream ended inside a line: the child ended while writing it,
    /// and its exit status says why.
    CutOff(String),
}

fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

/// The error of a child whose output cannot be read.
fn unreadable(error: &str) -> EngineError {
    runtime(format!("The wiki worker's output is unreadable: {error}"))
}

impl NativeRunner {
    /// The runner, with this process's own executable as the worker.
    ///
    /// # Errors
    ///
    /// No database URL, a URL that does not parse, or an executable path
    /// that cannot be found.
    pub fn new(settings: Settings) -> Result<Self, ConfigError> {
        let program = std::env::current_exe().map_err(|e| {
            ConfigError(format!(
                "the native runner cannot find its own executable to start workers: {e}"
            ))
        })?;
        Self::with_worker(
            settings,
            WorkerCommand {
                program,
                env: Vec::new(),
            },
        )
    }

    /// The runner with an explicit worker command.
    ///
    /// # Errors
    ///
    /// No database URL, or one that does not parse.
    pub fn with_worker(settings: Settings, worker: WorkerCommand) -> Result<Self, ConfigError> {
        let url = settings.database_url.as_ref().ok_or_else(|| {
            ConfigError(format!(
                "the native runner needs {}",
                crate::storage::DSN_ENV
            ))
        })?;
        let pool =
            crate::storage::lazy_pool(url.expose(), 2).map_err(|e| ConfigError(e.to_string()))?;
        Ok(Self {
            settings: Arc::new(settings),
            worker: Arc::new(worker),
            pool,
            cgroup: Cgroup::discover().map(Arc::new),
        })
    }

    /// The runner with `cgroup` as this process's cgroup (tests).
    #[must_use]
    pub fn with_cgroup(mut self, cgroup: Option<Cgroup>) -> Self {
        self.cgroup = cgroup.map(Arc::new);
        self
    }

    /// Run one tool.
    ///
    /// # Errors
    ///
    /// A refused tool or argument set, the worker's failure, or the stop
    /// line.
    pub async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        if tool != "generate_wiki" {
            return Err(runtime(format!(
                "{tool} is not supported by the native engine yet (ADR-0026 phase 6); run the fixture runner or the Python engine for it"
            )));
        }
        let arguments = prepare_arguments(tool, arguments)?;
        context.checkpoint()?;
        let directory = crate::generate::job_directory(
            &self.settings.ingest.scratch_path,
            &format!("job-{}", new_boot_id()),
        );
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&directory)
            .map_err(|e| {
                runtime(format!(
                    "The wiki worker has no scratch directory under {}: {e}",
                    self.settings.ingest.scratch_path.display()
                ))
            })?;
        let outcome = self.supervise(&directory, arguments, context).await;
        let removed = directory.clone();
        match tokio::task::spawn_blocking(move || std::fs::remove_dir_all(&removed)).await {
            Ok(Ok(())) => {}
            Ok(Err(error)) => {
                tracing::warn!(directory = %directory.display(), %error, "could not remove the job's scratch directory");
            }
            Err(error) => tracing::warn!(%error, "the scratch clean-up task failed"),
        }
        outcome
    }

    fn command(&self) -> Command {
        let mut command = Command::new(&self.worker.program);
        command
            .arg(WORKER_SUBCOMMAND)
            // Each parser thread reserves its stack in the address space
            // RLIMIT_AS counts, and each glibc arena reserves 64 MiB of it.
            .env(
                "RAYON_NUM_THREADS",
                self.settings.worker.threads.to_string(),
            )
            .env("MALLOC_ARENA_MAX", "2")
            // The limits this parent resolved (the memory default reads
            // the cgroup), so parent and child agree on them.
            .env(
                "ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES",
                self.settings.worker.memory_bytes.to_string(),
            )
            .env(
                "ELITEA_DEEPWIKI_WORKER_THREADS",
                self.settings.worker.threads.to_string(),
            )
            .envs(
                self.worker
                    .env
                    .iter()
                    .map(|(k, v)| (k.as_str(), v.as_str())),
            )
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        command
    }

    #[allow(clippy::too_many_lines)] // one supervision loop
    async fn supervise(
        &self,
        directory: &Path,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        let mut child = self
            .command()
            .spawn()
            .map_err(|e| runtime(format!("The wiki worker could not start: {e}")))?;
        let pid = child.id();
        let oom_kills_before = self.cgroup.as_deref().and_then(Cgroup::oom_kills);
        context.thinking(format!(
            "DeepWiki worker started (pid {})",
            pid.map_or_else(|| "?".to_owned(), |p| p.to_string())
        ));
        let request = json!({
            "arguments": Value::Object(arguments),
            "scratch": directory,
            "boot_id": process_boot_id(),
        });
        let mut stdin = child.stdin.take();
        if let Some(pipe) = stdin.as_mut() {
            let line = crate::pyjson::dumps(&request) + "\n";
            // A child that died at once closes its stdin; its exit status
            // says why, below.
            if let Err(error) = pipe.write_all(line.as_bytes()).await {
                tracing::warn!(%error, "could not hand the request to the wiki worker");
            }
            let _ = pipe.flush().await;
        }
        drop(request);
        let out_of_memory = Arc::new(AtomicBool::new(false));
        let logs = child
            .stderr
            .take()
            .map(|stderr| tokio::spawn(relay_logs(stderr, Arc::clone(&out_of_memory))));
        let (sender, mut lines) = mpsc::unbounded_channel();
        if let Some(stdout) = child.stdout.take() {
            tokio::spawn(read_lines(stdout, sender));
        }

        let stop = context.stop_signal();
        let mut stopped = false;
        let mut kill_at: Option<tokio::time::Instant> = None;
        let mut lines_open = true;
        let mut build_id: Option<String> = None;
        let mut last: Option<Result<Value, EngineError>> = None;
        let mut killed_by_parent = false;
        let mut cut_off: Option<String> = None;
        let status = loop {
            tokio::select! {
                message = lines.recv(), if lines_open => match message {
                    None => lines_open = false,
                    Some(Ok(line)) => {
                        relay(line, context, stopped, &mut build_id, &mut last);
                    }
                    Some(Err(ReadFailure::Unreadable(error))) => {
                        tracing::error!(%error, "the wiki worker's output is unreadable; killing it");
                        last = Some(Err(unreadable(&error)));
                        signal(pid, rustix::process::Signal::KILL);
                        killed_by_parent = true;
                        lines_open = false;
                    }
                    // The child is ending; its exit status says why.
                    Some(Err(ReadFailure::CutOff(error))) => {
                        cut_off = Some(error);
                        lines_open = false;
                    }
                },
                () = stop.stopped(), if !stopped => {
                    stopped = true;
                    signal(pid, rustix::process::Signal::TERM);
                    kill_at = Some(tokio::time::Instant::now() + KILL_AFTER);
                }
                () = sleep_until(kill_at), if kill_at.is_some() => {
                    tracing::warn!(pid, "the wiki worker did not stop within 3 s of SIGTERM; killing it");
                    signal(pid, rustix::process::Signal::KILL);
                    killed_by_parent = true;
                    kill_at = None;
                }
                status = child.wait() => break status,
            }
        };
        // What the child wrote before it ended may still be in the pipe.
        if lines_open {
            let drain = async {
                while let Some(message) = lines.recv().await {
                    match message {
                        Ok(line) => relay(line, context, stopped, &mut build_id, &mut last),
                        // The same as in the loop above (the child has
                        // ended, so there is nothing to kill).
                        Err(ReadFailure::Unreadable(error)) => {
                            tracing::error!(%error, "the wiki worker's output is unreadable");
                            last = Some(Err(unreadable(&error)));
                        }
                        Err(ReadFailure::CutOff(error)) => cut_off = Some(error),
                    }
                }
            };
            if tokio::time::timeout(Duration::from_secs(5), drain)
                .await
                .is_err()
            {
                tracing::warn!("the wiki worker's stdout stayed open after it ended");
            }
        }
        drop(stdin);
        if let Some(logs) = logs {
            let _ = tokio::time::timeout(Duration::from_secs(5), logs).await;
        }
        let outcome = match (last, stopped) {
            // A run that finished before it saw the stop keeps its result.
            (Some(Ok(result)), _) => Ok(result),
            (_, true) => Err(EngineError::cancelled()),
            (Some(Err(error)), false) => Err(error),
            (None, false) => {
                let facts = ExitFacts {
                    allocation_failed: out_of_memory.load(Ordering::Acquire),
                    killed_by_parent,
                    cut_off,
                    oom_kills: oom_kills_before
                        .zip(self.cgroup.as_deref().and_then(Cgroup::oom_kills)),
                };
                Err(exit_failure(&self.settings.worker, status, &facts))
            }
        };
        if let Some(build_id) = build_id {
            match delete_build(&self.pool, &build_id).await {
                Ok(true) => {
                    tracing::info!(build = %build_id, "deleted the build the wiki worker left");
                }
                Ok(false) => {}
                Err(error) => {
                    tracing::warn!(build = %build_id, %error, "could not delete the wiki worker's build; the sweep removes it");
                }
            }
        }
        outcome
    }
}

/// The failure of a child that ended without a last line, by its cause, in
/// this order: a failed allocation; SIGXCPU (the CPU limit); a SIGKILL
/// this parent did not send, which on Linux is the kernel's OOM killer
/// (the cgroup's `oom_kill` count confirms it when readable); a line cut
/// off or unreadable; any other end.
fn exit_failure(
    limits: &crate::config::WorkerSettings,
    status: std::io::Result<std::process::ExitStatus>,
    facts: &ExitFacts,
) -> EngineError {
    let cpu_signal = rustix::process::Signal::XCPU.as_raw();
    let kill_signal = rustix::process::Signal::KILL.as_raw();
    let signal = status.as_ref().ok().and_then(ExitStatusExt::signal);
    if facts.allocation_failed {
        return EngineError::new(
            ErrorType::Memory,
            format!(
                "The wiki worker ran out of memory: it reached its address-space limit of {} bytes (ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES)",
                limits.memory_bytes
            ),
        );
    }
    if signal == Some(cpu_signal) {
        return runtime(format!(
            "Wiki generation timeout: the worker used its CPU time limit of {} s (ELITEA_DEEPWIKI_WORKER_CPU_SECONDS)",
            limits.cpu_seconds
        ));
    }
    if signal == Some(kill_signal) && !facts.killed_by_parent {
        let cause = match facts.oom_kills {
            Some((before, after)) if after > before => {
                "the container reached its memory limit (the cgroup counted an OOM kill)"
            }
            _ => "the kernel's out-of-memory killer is the likely cause",
        };
        return EngineError::new(
            ErrorType::Memory,
            format!(
                "The wiki worker ran out of memory: it was killed (SIGKILL) and {cause}; lower ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES below the container limit or give the container more memory"
            ),
        );
    }
    if let Some(cut_off) = &facts.cut_off {
        return unreadable(cut_off);
    }
    match status {
        Ok(status) => runtime(format!(
            "The wiki worker process ended without a result ({status})"
        )),
        Err(error) => runtime(format!(
            "The wiki worker process could not be awaited: {error}"
        )),
    }
}

/// One line of the child's output.
fn relay(
    line: Value,
    context: &Context,
    stopped: bool,
    build_id: &mut Option<String>,
    last: &mut Option<Result<Value, EngineError>>,
) {
    let Value::Object(mut line) = line else {
        tracing::warn!("the wiki worker wrote a line that is not an object");
        return;
    };
    if let Some(Value::String(text)) = line.get("thinking") {
        if !stopped {
            context.thinking(text.clone());
        }
    } else if let Some(Value::String(text)) = line.get("token") {
        if !stopped {
            context.token(text.clone());
        }
    } else if let Some(Value::String(id)) = line.get(crate::worker::BUILD_KEY) {
        *build_id = Some(id.clone());
    } else if let Some(result) = line.remove("result") {
        *last = Some(Ok(result));
    } else if let Some(Value::Object(error)) = line.get("error") {
        let text = |key: &str| error.get(key).and_then(Value::as_str).unwrap_or_default();
        *last = Some(Err(EngineError::new(
            ErrorType::from_wire_name(text("error_type")),
            text("message"),
        )));
    } else {
        tracing::warn!("the wiki worker wrote a line this parent does not know");
    }
}

/// Signal the child. A child that already exited (a zombie until it is
/// waited for) ignores it; the pid is never reused before `wait` returns.
fn signal(pid: Option<u32>, signal: rustix::process::Signal) {
    let Some(pid) = pid
        .and_then(|p| i32::try_from(p).ok())
        .and_then(rustix::process::Pid::from_raw)
    else {
        return;
    };
    if let Err(error) = rustix::process::kill_process(pid, signal) {
        tracing::debug!(%error, "signalling the wiki worker failed (it has ended)");
    }
}

async fn sleep_until(deadline: Option<tokio::time::Instant>) {
    match deadline {
        Some(deadline) => tokio::time::sleep_until(deadline).await,
        None => std::future::pending().await,
    }
}

/// Read the child's NDJSON lines into `sender` until its stdout closes.
async fn read_lines(
    stdout: ChildStdout,
    sender: mpsc::UnboundedSender<Result<Value, ReadFailure>>,
) {
    let mut reader = BufReader::new(stdout);
    loop {
        let mut buffer = Vec::new();
        let limit = u64::try_from(MAX_RESULT_LINE).unwrap_or(u64::MAX) + 1;
        match (&mut reader)
            .take(limit)
            .read_until(b'\n', &mut buffer)
            .await
        {
            Ok(0) => return,
            Ok(_) if buffer.len() > MAX_RESULT_LINE => {
                let _ = sender.send(Err(ReadFailure::Unreadable(format!(
                    "a line is longer than {MAX_RESULT_LINE} bytes"
                ))));
                return;
            }
            Ok(_) => {
                let complete = buffer.ends_with(b"\n");
                let parsed = serde_json::from_slice::<Value>(&buffer).map_err(|e| {
                    if complete {
                        ReadFailure::Unreadable(format!("a line is not JSON: {e}"))
                    } else {
                        ReadFailure::CutOff(format!(
                            "its last line ends after {} bytes without a newline",
                            buffer.len()
                        ))
                    }
                });
                let failed = parsed.is_err();
                if sender.send(parsed).is_err() || failed || !complete {
                    return;
                }
            }
            Err(error) => {
                let _ = sender.send(Err(ReadFailure::Unreadable(error.to_string())));
                return;
            }
        }
    }
}

/// Copy the child's log lines to this process's stderr, noting an
/// allocation failure.
async fn relay_logs(stderr: ChildStderr, out_of_memory: Arc<AtomicBool>) {
    let mut reader = BufReader::new(stderr);
    let mut buffer = Vec::new();
    loop {
        buffer.clear();
        let limit = u64::try_from(MAX_LOG_LINE).unwrap_or(u64::MAX);
        match (&mut reader)
            .take(limit)
            .read_until(b'\n', &mut buffer)
            .await
        {
            Ok(0) | Err(_) => return,
            Ok(_) => {
                let text = String::from_utf8_lossy(&buffer);
                if text.contains(ALLOCATION_FAILED) && text.contains("failed") {
                    out_of_memory.store(true, Ordering::Release);
                }
                eprint!("{text}");
                if !text.ends_with('\n') {
                    eprintln!();
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runner::StopSignal;

    fn context() -> (
        Context,
        mpsc::UnboundedReceiver<super::super::Line>,
        StopSignal,
    ) {
        let (sender, receiver) = mpsc::unbounded_channel();
        let stop = StopSignal::default();
        (Context::new(sender, stop.clone()), receiver, stop)
    }

    /// A runner whose "worker" is a shell script.
    fn scripted(script: &str) -> (NativeRunner, PathBuf) {
        use std::os::unix::fs::PermissionsExt;
        let root = std::env::temp_dir().join(format!(
            "dw-native-{}-{}",
            std::process::id(),
            new_boot_id()
        ));
        std::fs::create_dir_all(&root).unwrap_or_else(|e| panic!("{e}"));
        let program = root.join("worker.sh");
        std::fs::write(&program, script).unwrap_or_else(|e| panic!("{e}"));
        std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o755))
            .unwrap_or_else(|e| panic!("{e}"));
        let scratch = root.join("scratch").to_string_lossy().into_owned();
        let settings = Settings::from_lookup(|name| match name {
            "ELITEA_DEEPWIKI_RUNNER" => Some("native".to_owned()),
            "ELITEA_DEEPWIKI_DATABASE_URL" => Some("postgresql://u:p@127.0.0.1:1/x".to_owned()),
            "ELITEA_DEEPWIKI_BUILD_OWNER" => Some("unit".to_owned()),
            "ELITEA_DEEPWIKI_SCRATCH_PATH" => Some(scratch.clone()),
            _ => None,
        })
        .unwrap_or_else(|e| panic!("{e}"));
        let runner = NativeRunner::with_worker(
            settings,
            WorkerCommand {
                program,
                env: Vec::new(),
            },
        )
        .unwrap_or_else(|e| panic!("{e}"));
        (runner, root)
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_worker_that_ignores_sigterm_is_killed_after_three_seconds() {
        // The worker ignores SIGTERM and never ends on its own.
        let (runner, root) = scripted(
            "#!/bin/sh\ntrap '' TERM\necho '{\"thinking\": \"working\"}'\nexec sleep 60\n",
        );
        let (context, mut receiver, stop) = context();
        let task = {
            let runner = runner.clone();
            tokio::spawn(async move { runner.run("generate_wiki", Map::new(), &context).await })
        };
        // Wait for the worker's own line, then stop.
        loop {
            match receiver.recv().await {
                Some(super::super::Line::Thinking(text)) if text == "working" => break,
                Some(_) => {}
                None => panic!("the run ended before the worker spoke"),
            }
        }
        let started = std::time::Instant::now();
        stop.request();
        let outcome = tokio::time::timeout(Duration::from_secs(20), task)
            .await
            .unwrap_or_else(|_| panic!("the worker was never killed"))
            .unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(outcome, Err(EngineError::cancelled()));
        assert!(started.elapsed() >= KILL_AFTER, "{:?}", started.elapsed());
        // The job's scratch directory is gone.
        let jobs = std::fs::read_dir(root.join("scratch/jobs")).map_or(0, Iterator::count);
        assert_eq!(jobs, 0);
        let _ = std::fs::remove_dir_all(&root);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_worker_that_dies_without_a_last_line_is_a_runtime_error() {
        let (runner, root) = scripted("#!/bin/sh\nexit 3\n");
        let (context, _receiver, _stop) = context();
        let outcome = runner.run("generate_wiki", Map::new(), &context).await;
        let error = outcome
            .err()
            .unwrap_or_else(|| panic!("a dead worker succeeded"));
        assert_eq!(error.error_type, ErrorType::Runtime);
        assert!(
            error.message.contains("ended without a result"),
            "{}",
            error.message
        );
        let _ = std::fs::remove_dir_all(&root);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn an_unreadable_line_after_the_exit_is_reported() {
        // The worker ends at once; a process that holds its stdout writes
        // a line that is not JSON after the parent saw the exit.
        let (runner, root) = scripted(
            "#!/bin/sh
(sleep 1; echo 'not json at all') &
exit 0
",
        );
        let (context, _receiver, _stop) = context();
        let outcome = runner.run("generate_wiki", Map::new(), &context).await;
        let error = outcome
            .err()
            .unwrap_or_else(|| panic!("an unreadable worker succeeded"));
        assert_eq!(error.error_type, ErrorType::Runtime);
        assert!(
            error.message.contains("output is unreadable"),
            "{}",
            error.message
        );
        let _ = std::fs::remove_dir_all(&root);
    }

    #[tokio::test(flavor = "multi_thread")]
    async fn a_sigkill_the_parent_did_not_send_is_out_of_memory() {
        // A partial last line, then the kernel's kill: the exit status, not
        // the cut-off line, is the cause.
        let (runner, root) =
            scripted("#!/bin/sh\nprintf '{\"thinking\": \"x\"}\\n{\"resu'\nkill -9 $$\n");
        let (context, _receiver, _stop) = context();
        let outcome = runner.run("generate_wiki", Map::new(), &context).await;
        let error = outcome
            .err()
            .unwrap_or_else(|| panic!("a killed worker succeeded"));
        assert_eq!(error.error_type, ErrorType::Memory, "{}", error.message);
        assert_eq!(error.category(), "out_of_memory");
        let _ = std::fs::remove_dir_all(&root);
    }

    #[test]
    fn the_exit_is_classified_by_its_cause() {
        use std::os::unix::process::ExitStatusExt;
        let limits = crate::config::WorkerSettings::default();
        let killed = || Ok(std::process::ExitStatus::from_raw(9));
        let classify = |facts: ExitFacts| exit_failure(&limits, killed(), &facts);
        // Not sent by the parent: memory; the cgroup count confirms it.
        let error = classify(ExitFacts {
            oom_kills: Some((3, 4)),
            cut_off: Some("cut".to_owned()),
            ..ExitFacts::default()
        });
        assert_eq!(error.error_type, ErrorType::Memory);
        assert!(
            error.message.contains("cgroup counted an OOM kill"),
            "{error}"
        );
        let error = classify(ExitFacts {
            oom_kills: Some((4, 4)),
            ..ExitFacts::default()
        });
        assert_eq!(error.error_type, ErrorType::Memory);
        assert!(error.message.contains("likely cause"), "{error}");
        // Sent by the parent: not memory; a cut-off line is the cause then.
        let error = classify(ExitFacts {
            killed_by_parent: true,
            cut_off: Some("its last line ends after 5 bytes".to_owned()),
            ..ExitFacts::default()
        });
        assert_eq!(error.error_type, ErrorType::Runtime);
        assert!(error.message.contains("output is unreadable"), "{error}");
        // SIGXCPU is the CPU limit; an allocation failure beats everything.
        let error = exit_failure(
            &limits,
            Ok(std::process::ExitStatus::from_raw(24)),
            &ExitFacts::default(),
        );
        assert_eq!(error.category(), "timeout_error");
        let error = classify(ExitFacts {
            allocation_failed: true,
            killed_by_parent: true,
            ..ExitFacts::default()
        });
        assert!(error.message.contains("address-space limit"), "{error}");
        // A plain exit.
        let error = exit_failure(
            &limits,
            Ok(std::process::ExitStatus::from_raw(3 << 8)),
            &ExitFacts::default(),
        );
        assert!(error.message.contains("ended without a result"), "{error}");
    }

    #[tokio::test]
    async fn the_query_tools_are_refused_until_their_port() {
        let (runner, root) = scripted("#!/bin/sh\nexit 0\n");
        let (context, _receiver, _stop) = context();
        for tool in ["ask", "deep_research", "resolve_wiki"] {
            let error = runner.run(tool, Map::new(), &context).await.err();
            assert!(
                error.is_some_and(|e| e.message.contains("not supported by the native engine yet")),
                "{tool}"
            );
        }
        let _ = std::fs::remove_dir_all(&root);
    }

    /// The product a Go integer expression such as `64*1024*1024` is.
    fn go_product(expression: &str) -> Option<usize> {
        expression
            .split('*')
            .map(|factor| factor.trim().parse::<usize>().ok())
            .product()
    }

    #[test]
    fn the_line_cap_is_the_go_hosts() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../elitea-subapp-host/internal/engine/engine.go");
        let source = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{e}"));
        let call = source
            .lines()
            .find_map(|line| line.trim().strip_prefix("scanner.Buffer("))
            .unwrap_or_else(|| panic!("no scanner.Buffer call in {}", path.display()));
        let maximum = call
            .trim_end()
            .strip_suffix(')')
            .and_then(|args| args.rsplit_once(','))
            .and_then(|(_, max)| go_product(max))
            .unwrap_or_else(|| panic!("unreadable scanner.Buffer call: {call}"));
        assert_eq!(maximum, MAX_RESULT_LINE);
    }

    #[test]
    fn the_worker_lines_are_relayed_or_kept() {
        let (context, mut receiver, _stop) = context();
        let (mut build, mut last) = (None, None);
        relay(
            json!({"thinking": "Cloning"}),
            &context,
            false,
            &mut build,
            &mut last,
        );
        relay(
            json!({"build": "b-1"}),
            &context,
            false,
            &mut build,
            &mut last,
        );
        relay(
            json!({"thinking": "late"}),
            &context,
            true,
            &mut build,
            &mut last,
        );
        relay(
            json!({"error": {"message": "boom", "error_type": "ValueError", "error_category": "invalid_input"}}),
            &context,
            false,
            &mut build,
            &mut last,
        );
        assert_eq!(
            receiver.try_recv().ok(),
            Some(super::super::Line::Thinking("Cloning".to_owned()))
        );
        assert!(receiver.try_recv().is_err());
        assert_eq!(build.as_deref(), Some("b-1"));
        assert_eq!(last, Some(Err(EngineError::new(ErrorType::Value, "boom"))));
        relay(
            json!({"result": {"success": true}}),
            &context,
            false,
            &mut build,
            &mut last,
        );
        assert_eq!(last, Some(Ok(json!({"success": true}))));
    }
}
