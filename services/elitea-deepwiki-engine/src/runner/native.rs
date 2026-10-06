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

/// The largest stdout line the parent reads from a child: the result line
/// carries every page.
pub const MAX_RESULT_LINE: usize = 512 * 1024 * 1024;

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
}

fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
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
        })
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
        let status = loop {
            tokio::select! {
                message = lines.recv(), if lines_open => match message {
                    None => lines_open = false,
                    Some(Ok(line)) => {
                        relay(line, context, stopped, &mut build_id, &mut last);
                    }
                    Some(Err(error)) => {
                        tracing::error!(%error, "the wiki worker's output is unreadable; killing it");
                        last = Some(Err(runtime(format!("The wiki worker's output is unreadable: {error}"))));
                        signal(pid, rustix::process::Signal::KILL);
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
                    kill_at = None;
                }
                status = child.wait() => break status,
            }
        };
        // What the child wrote before it ended may still be in the pipe.
        if lines_open {
            let drain = async {
                while let Some(message) = lines.recv().await {
                    if let Ok(line) = message {
                        relay(line, context, stopped, &mut build_id, &mut last);
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
            (None, false) => Err(self.exit_failure(status, out_of_memory.load(Ordering::Acquire))),
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

    /// The failure of a child that ended without a last line.
    fn exit_failure(
        &self,
        status: std::io::Result<std::process::ExitStatus>,
        out_of_memory: bool,
    ) -> EngineError {
        let limits = self.settings.worker;
        let cpu_signal = rustix::process::Signal::XCPU.as_raw();
        match status {
            _ if out_of_memory => EngineError::new(
                ErrorType::Memory,
                format!(
                    "The wiki worker ran out of memory: it reached its address-space limit of {} bytes (ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES)",
                    limits.memory_bytes
                ),
            ),
            Ok(status) if status.signal() == Some(cpu_signal) => runtime(format!(
                "Wiki generation timeout: the worker used its CPU time limit of {} s (ELITEA_DEEPWIKI_WORKER_CPU_SECONDS)",
                limits.cpu_seconds
            )),
            Ok(status) => runtime(format!(
                "The wiki worker process ended without a result ({status})"
            )),
            Err(error) => runtime(format!(
                "The wiki worker process could not be awaited: {error}"
            )),
        }
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
async fn read_lines(stdout: ChildStdout, sender: mpsc::UnboundedSender<Result<Value, String>>) {
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
                let _ = sender.send(Err(format!(
                    "a line is longer than {MAX_RESULT_LINE} bytes"
                )));
                return;
            }
            Ok(_) => {
                let parsed = serde_json::from_slice::<Value>(&buffer)
                    .map_err(|e| format!("a line is not JSON: {e}"));
                let failed = parsed.is_err();
                if sender.send(parsed).is_err() || failed {
                    return;
                }
            }
            Err(error) => {
                let _ = sender.send(Err(error.to_string()));
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
