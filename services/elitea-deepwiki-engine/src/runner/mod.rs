//! Tool runners: what actually answers a tool on the sidecar socket.
//!
//! * [`Runner::Unavailable`] — the default. It **refuses** every tool with a
//!   `resource_not_found` error rather than returning an empty success. A
//!   runner that is not wired must look broken, not idle.
//! * [`Runner::Fixture`] — canned results with paced progress, for a stack
//!   that proves the host → socket → composition → upload path without a
//!   model or a git host. The browser journeys run against it.
//!
//! The analysis engine itself (`native`) arrives in later ADR-0026 phases.

pub mod fixture;

use crate::errors::{EngineError, ErrorType};
use serde_json::{Map, Value};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tokio::sync::{Notify, mpsc};

/// The tools the sidecar serves. The Go host serves everything else itself.
pub const ENGINE_TOOLS: [&str; 4] = ["generate_wiki", "ask", "deep_research", "resolve_wiki"];

/// One NDJSON line of the engine's stream.
#[derive(Debug, Clone, PartialEq)]
pub enum Line {
    /// Progress text.
    Thinking(String),
    /// One fragment of the ANSWER (issue #701).
    Token(String),
    /// The tool's result dict — success or not; the host maps a failed one.
    Result(Value),
    /// A failure the host classifies by `error_type`.
    Error(EngineError),
}

impl Line {
    /// The JSON object this line is on the wire.
    #[must_use]
    pub fn to_json(&self) -> Value {
        match self {
            Self::Thinking(text) => serde_json::json!({ "thinking": text }),
            Self::Token(text) => serde_json::json!({ "token": text }),
            Self::Result(result) => serde_json::json!({ "result": result }),
            Self::Error(error) => error.to_line(),
        }
    }
}

/// The stop flag of one invocation, shared by the socket and the tool.
#[derive(Debug, Clone, Default)]
pub struct StopSignal {
    requested: Arc<AtomicBool>,
    notify: Arc<Notify>,
}

impl StopSignal {
    /// Ask the tool to stop at its next checkpoint, and wake any pause.
    pub fn request(&self) {
        self.requested.store(true, Ordering::SeqCst);
        self.notify.notify_waiters();
    }

    #[must_use]
    pub fn is_requested(&self) -> bool {
        self.requested.load(Ordering::SeqCst)
    }

    /// Resolve once a stop is requested (at once if it already was).
    ///
    /// A model call awaits this beside its request, so a stop aborts a
    /// call that may otherwise wait minutes for its first token.
    pub async fn stopped(&self) {
        loop {
            let notified = self.notify.notified();
            tokio::pin!(notified);
            // Registered before the flag is read, so a request that lands
            // between the two still wakes this waiter.
            notified.as_mut().enable();
            if self.is_requested() {
                return;
            }
            notified.await;
        }
    }
}

/// The hooks one tool run reports through.
#[derive(Debug, Clone)]
pub struct Context {
    lines: mpsc::UnboundedSender<Line>,
    stop: StopSignal,
}

impl Context {
    #[must_use]
    pub fn new(lines: mpsc::UnboundedSender<Line>, stop: StopSignal) -> Self {
        Self { lines, stop }
    }

    /// Report progress.
    pub fn thinking(&self, message: impl Into<String>) {
        // A closed channel means the reader went away; the stop flag is
        // already set by then and the next checkpoint ends the run.
        let _ = self.lines.send(Line::Thinking(message.into()));
    }

    /// Report one fragment of the answer. An empty fragment is DROPPED here:
    /// the poll drain discards an event with an empty message, so letting
    /// one through would spend a read-once event slot on nothing.
    pub fn token(&self, text: impl Into<String>) {
        let text = text.into();
        if !text.is_empty() {
            let _ = self.lines.send(Line::Token(text));
        }
    }

    /// Fail with the stop line once a stop was requested.
    ///
    /// # Errors
    ///
    /// [`EngineError::cancelled`] after a stop request.
    pub fn checkpoint(&self) -> Result<(), EngineError> {
        if self.stop.is_requested() {
            Err(EngineError::cancelled())
        } else {
            Ok(())
        }
    }

    /// Wait `duration`, or less if a stop arrives first.
    pub async fn pause(&self, duration: Duration) {
        if duration.is_zero() || self.stop.is_requested() {
            return;
        }
        let notified = self.stop.notify.notified();
        tokio::pin!(notified);
        notified.as_mut().enable();
        if self.stop.is_requested() {
            return;
        }
        tokio::select! {
            () = tokio::time::sleep(duration) => {}
            () = notified => {}
        }
    }
}

/// Reader-selected wiki pages (`context_paths`) and their pinned version.
pub const PATHS_PARAM: &str = "context_paths";
pub const VERSION_PARAM: &str = "context_wiki_version_id";
/// Reader-uploaded text files.
pub const EXTRA_CONTEXT_PARAM: &str = "extra_context";

/// The argument set a tool sees, after the attachment keys.
///
/// The Go host resolves `context_paths` and `extra_context` in front of its
/// tool table (`run/contextpaths.go`, `run/extracontext.go`) and REMOVES
/// both keys, so a request that came through the host arrives here with
/// nothing left to do; the version key is dropped as the Python sidecar's
/// `consume` drops it. A sidecar called directly with attachments still in
/// place is REFUSED rather than silently answering without them: resolving
/// them here needs the artifact client, which lands with the native `ask`
/// (ADR-0026 phase 6).
///
/// # Errors
///
/// A `ValueError` (Python's `ContextRefused`) when an attachment reached
/// the engine unresolved.
pub fn prepare_arguments(
    tool: &str,
    mut arguments: Map<String, Value>,
) -> Result<Map<String, Value>, EngineError> {
    for key in [PATHS_PARAM, EXTRA_CONTEXT_PARAM] {
        let present = arguments.get(key).is_some_and(crate::source::py_truthy);
        if !present {
            continue;
        }
        if !matches!(tool, "ask" | "deep_research") {
            let what = if key == PATHS_PARAM { "pages" } else { "files" };
            return Err(EngineError::new(
                ErrorType::Value,
                format!("{key} is not supported by {tool}; attach {what} to ask or deep_research"),
            ));
        }
        return Err(EngineError::new(
            ErrorType::Value,
            format!(
                "{key} reached the engine unresolved; the sub-application host resolves it before this socket"
            ),
        ));
    }
    arguments.remove(PATHS_PARAM);
    arguments.remove(VERSION_PARAM);
    arguments.remove(EXTRA_CONTEXT_PARAM);
    Ok(arguments)
}

/// The runner a sidecar serves.
#[derive(Debug, Clone)]
pub enum Runner {
    Unavailable,
    Fixture(fixture::FixtureRunner),
}

impl Runner {
    /// The name `GET /engine/health` reports.
    #[must_use]
    pub fn name(&self) -> &'static str {
        match self {
            Self::Unavailable => "unavailable",
            Self::Fixture(_) => "fixture",
        }
    }

    /// Run one tool with the keyword set the host derived.
    ///
    /// # Errors
    ///
    /// The tool's failure, or the stop line after a stop request.
    pub async fn run(
        &self,
        tool: &str,
        arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        match self {
            Self::Unavailable => Err(EngineError::new(
                ErrorType::FileNotFound,
                format!(
                    "No DeepWiki tool runner is configured, so '{tool}' cannot run. \
                     Set ELITEA_DEEPWIKI_RUNNER=fixture for canned results; the native \
                     analysis engine is not part of this build yet."
                ),
            )),
            Self::Fixture(runner) => runner.run(tool, arguments, context).await,
        }
    }

    /// Publish a completed generation's index. Neither runner here has one.
    #[allow(clippy::unused_async)] // The native runner's publish is async.
    pub async fn publish(&self, _result: &Value, _context: &Context) {}
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_pause_ends_when_a_stop_arrives() {
        let (sender, _receiver) = mpsc::unbounded_channel();
        let stop = StopSignal::default();
        let context = Context::new(sender, stop.clone());
        let started = std::time::Instant::now();
        let pause = tokio::spawn(async move { context.pause(Duration::from_secs(30)).await });
        tokio::time::sleep(Duration::from_millis(20)).await;
        stop.request();
        assert!(pause.await.is_ok());
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[test]
    fn an_empty_token_never_reaches_the_channel() {
        let (sender, mut receiver) = mpsc::unbounded_channel();
        let context = Context::new(sender, StopSignal::default());
        context.token("");
        context.token("x");
        assert_eq!(receiver.try_recv().ok(), Some(Line::Token("x".to_owned())));
        assert!(receiver.try_recv().is_err());
    }
}
