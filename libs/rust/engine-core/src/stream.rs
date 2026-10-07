//! The NDJSON stream one engine tool run reports through, and its stop flag.
//!
//! Every engine sidecar answers `POST /engine/invoke` with the same lines —
//! `thinking`, `token`, then one `result` or `error` — and every long call
//! inside a tool (a model request, a clone, a worker child) waits on the same
//! stop flag. These types are that contract, shared by the sidecar server and
//! the clients that must abort when a stop arrives.

use crate::errors::EngineError;
use serde_json::Value;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;
use tokio::sync::{Notify, mpsc};

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

    /// The invocation's stop signal (a clone sharing the flag), for the
    /// calls that wait on it: model requests, the clone watchdog, the
    /// worker child's supervisor.
    #[must_use]
    pub fn stop_signal(&self) -> StopSignal {
        self.stop.clone()
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
