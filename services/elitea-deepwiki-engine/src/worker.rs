//! The `generate_wiki` worker child (`elitea-deepwiki-engine worker`,
//! ADR-0026 decision 10). The parent's half is [`crate::runner::native`].
//!
//! # Protocol
//!
//! * stdin, first line: `{"arguments": {…}, "scratch": "<job dir>",
//!   "boot_id": "<parent run>"}`. The arguments carry credentials, which is
//!   why they come this way and not through `argv` or the environment.
//!   Stdin then stays open; its end (the parent went away) is a stop.
//! * stdout: NDJSON, the socket's own line shapes — `{"thinking": …}`,
//!   `{"token": …}` — plus `{"build": "<id>"}` once a build is open, and
//!   last exactly one `{"result": …}` or `{"error": {…}}`.
//! * stderr: the logs.
//! * SIGTERM is a stop: the run ends at its next checkpoint, abandons its
//!   build and writes the stop line. The parent kills it 3 s later anyway.
//!
//! # Limits
//!
//! Before it reads its request the child limits itself (`setrlimit`, through
//! `rustix`'s safe wrapper): `RLIMIT_AS` to
//! `ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES` and `RLIMIT_CPU` to
//! `ELITEA_DEEPWIKI_WORKER_CPU_SECONDS` (hard limit 10 s above: SIGXCPU,
//! then SIGKILL). A limit is only lowered, never raised past the hard limit
//! the process already has. On Linux the child also asks for SIGKILL when
//! its parent dies (`PR_SET_PDEATHSIG`). macOS does not enforce
//! `RLIMIT_AS`; the call is still made there.

use crate::config::{Settings, WorkerSettings};
use crate::errors::{EngineError, ErrorType};
use crate::generate::{self, Job};
use crate::runner::{Context, Line, StopSignal};
use serde_json::{Map, Value, json};
use std::path::PathBuf;
use std::process::ExitCode;
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::sync::mpsc;

/// The key of the line that names the open build.
pub const BUILD_KEY: &str = "build";

/// The largest request the child reads.
const MAX_REQUEST_BYTES: u64 = 64 * 1024 * 1024;

/// The CPU hard limit's margin over the soft one.
const CPU_HARD_MARGIN: u64 = 10;

/// What the parent sends.
#[derive(Debug)]
struct WorkerRequest {
    arguments: Map<String, Value>,
    scratch: PathBuf,
    boot_id: Option<String>,
}

fn parse_request(line: &[u8]) -> Result<WorkerRequest, EngineError> {
    let invalid = |what: &str| {
        EngineError::new(
            ErrorType::Runtime,
            format!("The wiki worker received a malformed request: {what}"),
        )
    };
    let Ok(Value::Object(mut request)) = serde_json::from_slice::<Value>(line) else {
        return Err(invalid("not a JSON object"));
    };
    let Some(Value::Object(arguments)) = request.remove("arguments") else {
        return Err(invalid("no arguments"));
    };
    let Some(Value::String(scratch)) = request.remove("scratch") else {
        return Err(invalid("no scratch directory"));
    };
    let boot_id = match request.remove("boot_id") {
        Some(Value::String(id)) if !id.is_empty() => Some(id),
        _ => None,
    };
    Ok(WorkerRequest {
        arguments,
        scratch: PathBuf::from(scratch),
        boot_id,
    })
}

/// A soft and hard limit, each at most the hard limit the process already
/// has (an unprivileged process cannot raise it).
fn lowered(current: rustix::process::Rlimit, soft: u64, hard: u64) -> rustix::process::Rlimit {
    let maximum = current.maximum.map_or(hard, |existing| existing.min(hard));
    rustix::process::Rlimit {
        current: Some(soft.min(maximum)),
        maximum: Some(maximum),
    }
}

/// Apply the worker's limits to this process.
///
/// # Errors
///
/// A `setrlimit` the system refused (on Linux; elsewhere it is logged).
pub fn apply_limits(limits: &WorkerSettings) -> std::io::Result<()> {
    use rustix::process::{Resource, getrlimit, setrlimit};
    let memory = lowered(
        getrlimit(Resource::As),
        limits.memory_bytes,
        limits.memory_bytes,
    );
    if let Err(error) = setrlimit(Resource::As, memory) {
        if cfg!(target_os = "linux") {
            return Err(error.into());
        }
        tracing::warn!(%error, "RLIMIT_AS is not available here; the worker runs without a memory cap");
    }
    let cpu = lowered(
        getrlimit(Resource::Cpu),
        limits.cpu_seconds,
        limits.cpu_seconds.saturating_add(CPU_HARD_MARGIN),
    );
    setrlimit(Resource::Cpu, cpu)?;
    #[cfg(target_os = "linux")]
    rustix::process::set_parent_process_death_signal(Some(rustix::process::Signal::KILL))?;
    Ok(())
}

/// Run the worker: limits, the request, the pipeline, the last line.
pub async fn run(settings: &Settings) -> ExitCode {
    let (output, writer) = start_writer();
    if let Err(error) = apply_limits(&settings.worker) {
        let failure = EngineError::new(
            ErrorType::Runtime,
            format!("The wiki worker could not apply its resource limits: {error}"),
        );
        let _ = output.send(failure.to_line());
        drop(output);
        let _ = writer.await;
        return ExitCode::FAILURE;
    }
    let mut stdin = BufReader::new(tokio::io::stdin());
    let mut line = Vec::new();
    let read = (&mut stdin)
        .take(MAX_REQUEST_BYTES)
        .read_until(b'\n', &mut line)
        .await;
    let request = match read {
        Ok(0) | Err(_) => Err(EngineError::new(
            ErrorType::Runtime,
            "The wiki worker received no request",
        )),
        Ok(_) => parse_request(&line),
    };
    line.clear();
    let request = match request {
        Ok(request) => request,
        Err(error) => {
            let _ = output.send(error.to_line());
            drop(output);
            let _ = writer.await;
            return ExitCode::FAILURE;
        }
    };

    let stop = StopSignal::default();
    watch_parent(stdin, stop.clone());
    watch_terminate(stop.clone());

    let (lines, mut received) = mpsc::unbounded_channel::<Line>();
    let context = Context::new(lines, stop);
    let forward = output.clone();
    let forwarder = tokio::spawn(async move {
        while let Some(line) = received.recv().await {
            if forward.send(line.to_json()).is_err() {
                return;
            }
        }
    });
    let build_lines = output.clone();
    let on_build = move |id: &str| {
        let _ = build_lines.send(json!({ BUILD_KEY: id }));
    };
    let job = Job {
        scratch: &request.scratch,
        boot_id: request.boot_id.as_deref(),
        on_build: &on_build,
    };
    let outcome = generate::generate_wiki(&request.arguments, settings, &context, &job).await;
    drop(request);
    // Progress first, the last line last.
    drop(context);
    if tokio::time::timeout(std::time::Duration::from_secs(5), forwarder)
        .await
        .is_err()
    {
        tracing::warn!("a progress sender outlived the run; its lines are dropped");
    }
    let last = match outcome {
        Ok(result) => json!({ "result": result }),
        Err(error) => {
            tracing::warn!(
                error_type = error.error_type.wire_name(),
                "generate_wiki failed"
            );
            error.to_line()
        }
    };
    let _ = output.send(last);
    drop(output);
    drop(on_build);
    let _ = writer.await;
    ExitCode::SUCCESS
}

/// The stdout writer: one JSON object per line, flushed per line.
fn start_writer() -> (mpsc::UnboundedSender<Value>, tokio::task::JoinHandle<()>) {
    let (sender, mut receiver) = mpsc::unbounded_channel::<Value>();
    let writer = tokio::spawn(async move {
        let mut stdout = tokio::io::stdout();
        while let Some(line) = receiver.recv().await {
            let text = crate::pyjson::dumps(&line) + "\n";
            if stdout.write_all(text.as_bytes()).await.is_err() || stdout.flush().await.is_err() {
                // The parent is gone; nothing will read the rest.
                return;
            }
        }
    });
    (sender, writer)
}

/// The end of stdin means the parent went away: stop.
fn watch_parent(mut stdin: BufReader<tokio::io::Stdin>, stop: StopSignal) {
    tokio::spawn(async move {
        let mut sink = [0_u8; 1024];
        loop {
            match stdin.read(&mut sink).await {
                Ok(0) | Err(_) => break,
                Ok(_) => {}
            }
        }
        stop.request();
    });
}

/// SIGTERM is a stop.
fn watch_terminate(stop: StopSignal) {
    tokio::spawn(async move {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut terminate) => {
                if terminate.recv().await.is_some() {
                    tracing::info!("SIGTERM: stopping at the next checkpoint");
                    stop.request();
                }
            }
            Err(error) => tracing::warn!(%error, "no SIGTERM handler; a stop kills the worker"),
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_request_names_its_arguments_and_scratch() {
        let parsed = parse_request(
            br#"{"arguments": {"query": "q"}, "scratch": "/s/job-1", "boot_id": "b"}"#,
        );
        let Ok(parsed) = parsed else {
            panic!("refused");
        };
        assert_eq!(parsed.scratch, PathBuf::from("/s/job-1"));
        assert_eq!(parsed.boot_id.as_deref(), Some("b"));
        assert_eq!(parsed.arguments.get("query"), Some(&json!("q")));
        for bad in [
            &b"[]"[..],
            br#"{"scratch": "/s"}"#,
            br#"{"arguments": {}}"#,
            b"not json",
        ] {
            assert!(parse_request(bad).is_err());
        }
    }

    #[test]
    fn limits_are_only_lowered() {
        let unlimited = rustix::process::Rlimit {
            current: None,
            maximum: None,
        };
        let applied = lowered(unlimited, 100, 110);
        assert_eq!((applied.current, applied.maximum), (Some(100), Some(110)));
        let capped = rustix::process::Rlimit {
            current: Some(50),
            maximum: Some(60),
        };
        let applied = lowered(capped, 100, 110);
        assert_eq!((applied.current, applied.maximum), (Some(60), Some(60)));
    }
}
