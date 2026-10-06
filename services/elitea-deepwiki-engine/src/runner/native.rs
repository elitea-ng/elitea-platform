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
//! `ask`, `deep_research` and `resolve_wiki` run IN this process, not in a
//! worker: they are I/O-bound (model and database calls), read only
//! PostgreSQL (ADR-0026 decision 5) and stop at every model and tool step
//! ([`crate::ask`]).

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
    /// For deleting the build of a child that was killed, and for the
    /// query tools' reads.
    pool: PgPool,
    /// The query tools' step limits, read once at start so a bad value
    /// fails the start rather than a request.
    query_limits: crate::ask::Limits,
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
        let query_limits =
            crate::ask::Limits::from_env().map_err(|e| ConfigError(e.message.clone()))?;
        Ok(Self {
            settings: Arc::new(settings),
            worker: Arc::new(worker),
            pool,
            query_limits,
        })
    }

    /// `ask`, `deep_research` or `resolve_wiki`, in this process.
    ///
    /// No repository analysis is passed: the Python ask loaded it from the
    /// analysis store on scratch, which a query replica does not have
    /// (ADR-0026 decision 5); the README records it.
    async fn query(
        &self,
        tool: &str,
        arguments: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        let transport =
            crate::llm::Transport::new(&crate::llm::TransportSettings::from(&self.settings.model))?;
        let deps = crate::ask::QueryDeps {
            pool: self.pool.clone(),
            transport,
            embedding_options: crate::llm::EmbeddingOptions::from(&self.settings.model),
            limits: self.query_limits,
            clock: crate::ask::agent::Clock::System,
        };
        crate::ask::run_tool(tool, arguments, &deps, None, context).await
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
        if matches!(tool, "ask" | "deep_research" | "resolve_wiki") {
            let arguments = prepare_arguments(tool, arguments)?;
            context.checkpoint()?;
            return self.query(tool, &arguments, context).await;
        }
        if tool != "generate_wiki" {
            return Err(EngineError::new(
                ErrorType::Key,
                format!("Unknown tool: {tool}"),
            ));
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

    #[tokio::test]
    async fn the_query_tools_run_in_process_and_never_spawn_a_worker() {
        // A worker that would fail loudly if spawned: the query tools must
        // reach `ask::run_tool`, whose first step (the model settings)
        // refuses an argument set with no llm_settings.
        let (runner, root) = scripted("#!/bin/sh\necho spawned >&2\nexit 7\n");
        let (context, _receiver, _stop) = context();
        for tool in ["ask", "deep_research", "resolve_wiki"] {
            let mut arguments = Map::new();
            arguments.insert("question".to_owned(), Value::String("q".to_owned()));
            let outcome = runner.run(tool, arguments, &context).await;
            let text = match &outcome {
                Ok(value) => value.to_string(),
                Err(error) => error.message.clone(),
            };
            assert!(!text.contains("ended without a result"), "{tool}: {text}");
            assert!(
                !text.contains("not supported by the native engine"),
                "{tool}: {text}"
            );
        }
        let unknown = runner.run("list_wikis", Map::new(), &context).await.err();
        assert_eq!(unknown.map(|e| e.error_type), Some(ErrorType::Key));
        let _ = std::fs::remove_dir_all(&root);
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
