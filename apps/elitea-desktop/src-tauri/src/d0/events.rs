//! The `agent://event` stream to the webview (IPC.md, "Events").
//!
//! Every event of one turn carries the turn id and a sequence number that
//! starts at 0 and grows by one per event, in emission order: the UI can
//! detect a gap or a reorder. Payloads never carry a token, a toolkit
//! secret or a raw HTTP body.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

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

/// A phase before the model's first token that takes longer than this is
/// logged as a warning.
pub const SLOW_PHASE: Duration = Duration::from_secs(2);

/// The phases a turn times (`status` `timings`, IPC.md "Events").
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Timed {
    /// `resolved_version` (or the cached definition and its revalidation).
    Resolving,
    /// The conversation lookup: the answering participant and UUID.
    Participant,
    /// From `status` `starting` to the local turn's start (the session's
    /// seed, when it needs one, included).
    Starting,
    /// From `status` `running` to the model's first streamed chunk.
    FirstToken,
    /// The wall time some tool call ran (overlapping calls counted once).
    Tools,
    /// The commit request.
    Committing,
}

impl Timed {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Resolving => "resolving",
            Self::Participant => "participant",
            Self::Starting => "starting",
            Self::FirstToken => "first_token",
            Self::Tools => "tools",
            Self::Committing => "committing",
        }
    }

    /// Paid before the model's first token: what a slow start is made of.
    const fn before_first_token(self) -> bool {
        matches!(
            self,
            Self::Resolving | Self::Participant | Self::Starting | Self::FirstToken
        )
    }
}

fn millis(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

/// The turn's clock: phases measured, not yet sent, and the tools' busy time.
struct Clock {
    started: Instant,
    /// Measured since the last `status` event: the next one carries them.
    pending: Vec<Value>,
    /// Every phase measured, in order, for the turn's log line.
    all: Vec<(Timed, u64)>,
    /// When the model was first asked (`status` `running`).
    running_since: Option<Instant>,
    first_token: bool,
    tools_running: usize,
    tools_since: Option<Instant>,
    tools_busy: Duration,
    tools_ran: bool,
}

/// One turn's event channel: numbers and emits in one critical section, so
/// sequence order is emission order even when tools run concurrently.
pub struct TurnEvents {
    turn_id: String,
    emitter: Arc<dyn EventEmitter>,
    seq: Mutex<u64>,
    clock: Mutex<Clock>,
}

impl TurnEvents {
    #[must_use]
    pub fn new(turn_id: String, emitter: Arc<dyn EventEmitter>) -> Self {
        Self {
            turn_id,
            emitter,
            seq: Mutex::new(0),
            clock: Mutex::new(Clock {
                started: Instant::now(),
                pending: Vec::new(),
                all: Vec::new(),
                running_since: None,
                first_token: false,
                tools_running: 0,
                tools_since: None,
                tools_busy: Duration::ZERO,
                tools_ran: false,
            }),
        }
    }

    fn clock(&self) -> std::sync::MutexGuard<'_, Clock> {
        self.clock
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// `phase` took `took`: the next `status` event carries it as
    /// `{phase, ms_since_turn_start, phase_ms}`. A phase before the first
    /// token that took longer than [`SLOW_PHASE`] is logged.
    pub fn timed(&self, phase: Timed, took: Duration) {
        let phase_ms = millis(took);
        let mut clock = self.clock();
        let since_start = millis(clock.started.elapsed());
        clock.pending.push(json!({
            "phase": phase.as_str(),
            "ms_since_turn_start": since_start,
            "phase_ms": phase_ms,
        }));
        clock.all.push((phase, phase_ms));
        drop(clock);
        if phase.before_first_token() && took > SLOW_PHASE {
            log::warn!(
                "a local turn's {} phase took {phase_ms} ms (before the first token)",
                phase.as_str()
            );
        }
    }

    /// The model's first streamed chunk of the turn arrived.
    pub fn first_token(&self) {
        let took = {
            let mut clock = self.clock();
            match (clock.first_token, clock.running_since) {
                (false, Some(since)) => {
                    clock.first_token = true;
                    since.elapsed()
                }
                _ => return,
            }
        };
        self.timed(Timed::FirstToken, took);
    }

    /// A tool call began; with [`Self::tool_ended`], the tools' wall time.
    pub fn tool_began(&self) {
        let mut clock = self.clock();
        if clock.tools_running == 0 {
            clock.tools_since = Some(Instant::now());
        }
        clock.tools_running += 1;
        clock.tools_ran = true;
    }

    pub fn tool_ended(&self) {
        let mut clock = self.clock();
        clock.tools_running = clock.tools_running.saturating_sub(1);
        if clock.tools_running == 0
            && let Some(since) = clock.tools_since.take()
        {
            clock.tools_busy += since.elapsed();
        }
    }

    /// The run ended: the tools' wall time, when any ran.
    pub fn tools_done(&self) {
        let busy = {
            let clock = self.clock();
            clock.tools_ran.then_some(clock.tools_busy)
        };
        if let Some(busy) = busy {
            self.timed(Timed::Tools, busy);
        }
    }

    /// One line with every phase of the turn: what the live check reads.
    pub fn log_timings(&self) {
        let clock = self.clock();
        let phases: Vec<String> = clock
            .all
            .iter()
            .map(|(phase, ms)| format!("{}={ms}ms", phase.as_str()))
            .collect();
        log::info!(
            "local turn timings: {} (total {} ms)",
            phases.join(" "),
            millis(clock.started.elapsed())
        );
    }

    /// The pending timings, on a `status` payload.
    fn with_timings(&self, mut payload: Value) -> Value {
        let pending = std::mem::take(&mut self.clock().pending);
        if !pending.is_empty() {
            payload["timings"] = Value::Array(pending);
        }
        payload
    }

    /// `status` with `payload`, its timings added.
    fn send_status(&self, payload: Value) {
        let payload = self.with_timings(payload);
        self.send("status", payload);
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
        self.send_status(payload);
    }

    /// `status` `running`, naming the AGENTS.md files the turn applies
    /// (`project_instructions`, only when there are some).
    pub fn running(&self, project_instructions: &[String]) {
        let mut payload = json!({ "phase": Phase::Running.as_str() });
        if !project_instructions.is_empty() {
            payload["project_instructions"] = json!(project_instructions);
        }
        self.clock().running_since = Some(Instant::now());
        self.send_status(payload);
    }

    /// `status` `running` again: the run goes on past its step limit
    /// (`continued`: the how-many-th time), from the host's own prompt
    /// (`host_prompt`), which is never the person's message.
    pub fn continued(&self, steps: u32, continued: u32) {
        self.send_status(json!({
                "phase": Phase::Running.as_str(),
                "message": format!("Continuing for {steps} more steps."),
                "continued": continued,
            "host_prompt": elitea_agent_runtime::step_limit::CONTINUE_PROMPT,
        }));
    }

    /// `status` with a machine-readable `note` (`step_limit_reached`).
    pub fn noted(&self, phase: Phase, message: &str, note: &str) {
        self.send_status(json!({ "phase": phase.as_str(), "message": message, "note": note }));
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_status_carries_the_phases_timed_since_the_last_one() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter.clone());
        events.status(Phase::Resolving, None);
        events.timed(Timed::Resolving, Duration::from_millis(120));
        events.timed(Timed::Participant, Duration::from_millis(80));
        events.status(Phase::Starting, None);
        events.status(Phase::Starting, Some("again"));
        events.running(&[]);
        // Two overlapping tool calls, then a later one: their union.
        events.tool_began();
        events.tool_began();
        events.first_token();
        events.first_token();
        events.tool_ended();
        events.tool_ended();
        events.tools_done();
        events.status(Phase::Committing, None);
        let all = emitter.all();
        assert!(all[0].payload.get("timings").is_none());
        let starting = all[1].payload["timings"].as_array().unwrap();
        assert_eq!(starting.len(), 2);
        assert_eq!(starting[0]["phase"], "resolving");
        assert_eq!(starting[0]["phase_ms"], 120);
        assert!(starting[0]["ms_since_turn_start"].is_u64());
        assert_eq!(starting[1]["phase"], "participant");
        assert!(all[2].payload.get("timings").is_none(), "sent once");
        let committing = all[4].payload["timings"].as_array().unwrap();
        let phases: Vec<&str> = committing
            .iter()
            .map(|t| t["phase"].as_str().unwrap())
            .collect();
        assert_eq!(
            phases,
            ["first_token", "tools"],
            "the first token is timed once"
        );
    }
}
