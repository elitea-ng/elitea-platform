//! The `agent://event` stream to the webview (IPC.md, "Events").
//!
//! Every event of one turn carries the turn id and a sequence number that
//! starts at 0 and grows by one per event, in emission order: the UI can
//! detect a gap or a reorder. Payloads never carry a token, a toolkit
//! secret or a raw HTTP body.

use std::sync::{Arc, Mutex};

use serde::Serialize;
use serde_json::{Value, json};

/// The event name the main window listens to.
pub const EVENT_NAME: &str = "agent://event";

/// One event, as the webview receives it.
#[derive(Clone, Debug, Serialize, PartialEq)]
pub struct AgentEvent {
    pub turn_id: String,
    pub seq: u64,
    pub kind: &'static str,
    pub payload: Value,
}

/// Where events go: the main window in the app, a vector in tests.
pub trait EventEmitter: Send + Sync {
    fn emit(&self, event: AgentEvent);
}

/// A turn's phases, as `status` events name them.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    Resolving,
    Starting,
    Running,
    Committing,
    Done,
    Cancelled,
    Error,
}

impl Phase {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Resolving => "resolving",
            Self::Starting => "starting",
            Self::Running => "running",
            Self::Committing => "committing",
            Self::Done => "done",
            Self::Cancelled => "cancelled",
            Self::Error => "error",
        }
    }
}

/// One turn's event channel: numbers and emits in one critical section, so
/// sequence order is emission order even when tools run concurrently.
pub struct TurnEvents {
    turn_id: String,
    emitter: Arc<dyn EventEmitter>,
    seq: Mutex<u64>,
}

impl TurnEvents {
    #[must_use]
    pub fn new(turn_id: String, emitter: Arc<dyn EventEmitter>) -> Self {
        Self {
            turn_id,
            emitter,
            seq: Mutex::new(0),
        }
    }

    #[must_use]
    pub fn turn_id(&self) -> &str {
        &self.turn_id
    }

    pub fn send(&self, kind: &'static str, payload: Value) {
        let Ok(mut seq) = self.seq.lock() else {
            return;
        };
        self.emitter.emit(AgentEvent {
            turn_id: self.turn_id.clone(),
            seq: *seq,
            kind,
            payload,
        });
        *seq += 1;
    }

    pub fn status(&self, phase: Phase, message: Option<&str>) {
        let mut payload = json!({ "phase": phase.as_str() });
        if let Some(message) = message {
            payload["message"] = Value::String(message.to_owned());
        }
        self.send("status", payload);
    }

    pub fn text_delta(&self, text: &str) {
        if !text.is_empty() {
            self.send("text_delta", json!({ "text": text }));
        }
    }

    pub fn error(&self, code: &str, message: &str) {
        self.send("error", json!({ "code": code, "message": message }));
    }
}

/// Collects events, for tests.
#[cfg(test)]
pub type EventHook = Box<dyn Fn(&AgentEvent) + Send + Sync>;

#[cfg(test)]
#[derive(Default)]
pub struct VecEmitter {
    pub events: Mutex<Vec<AgentEvent>>,
    /// Called with each event after it is stored (an approval answerer).
    pub on_event: Mutex<Option<EventHook>>,
}

#[cfg(test)]
impl VecEmitter {
    pub fn all(&self) -> Vec<AgentEvent> {
        self.events.lock().expect("lock").clone()
    }

    pub fn kinds(&self) -> Vec<&'static str> {
        self.all().iter().map(|event| event.kind).collect()
    }
}

#[cfg(test)]
impl EventEmitter for VecEmitter {
    fn emit(&self, event: AgentEvent) {
        self.events.lock().expect("lock").push(event.clone());
        if let Some(callback) = self.on_event.lock().expect("lock").as_ref() {
            callback(&event);
        }
    }
}
