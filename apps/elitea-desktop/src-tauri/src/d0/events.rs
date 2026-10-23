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

    /// A background process's event on the app channel
    /// (`process://event`, #1223); nowhere by default.
    fn emit_process(&self, _event: super::processes::ProcessEvent) {}
}

/// A turn's phases, as `status` events name them.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    Resolving,
    Starting,
    Running,
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
    context: Mutex<Option<ContextMeter>>,
    usage: Mutex<Ledger>,
}

/// One model call's token usage as the gateway reported it (#1227).
/// `cached` and `cache_write` are parts of `prompt` (the gateway counts a
/// cache read or write among the prompt tokens and bills it at the input
/// price), `reasoning` a part of `completion`.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct CallUsage {
    pub prompt: u64,
    pub completion: u64,
    pub total: u64,
    /// `prompt_tokens_details.cached_tokens`: read from the provider's cache.
    pub cached: Option<u64>,
    /// `prompt_tokens_details.cache_write_tokens`: written to it (Anthropic).
    pub cache_write: Option<u64>,
    /// `completion_tokens_details.reasoning_tokens`.
    pub reasoning: Option<u64>,
}

/// Whose model call it was. Every call goes to the gateway on the turn's
/// execution id, so every one counts toward the turn's `usage_total`;
/// only the turn's own calls send a `usage` event each.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum UsageLane {
    /// The agent's own model step.
    #[default]
    Turn,
    /// A context compaction's summary call (quiet).
    Summary,
    /// A sub-agent's model step (#1226, quiet): also counted apart.
    SubAgent,
}

/// What the calls of one lane, or of the whole turn, added up to.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Totals {
    prompt: u64,
    completion: u64,
    cached: u64,
    cache_write: u64,
    reasoning: u64,
    calls: u64,
}

impl Totals {
    fn add(&mut self, usage: &CallUsage) {
        self.prompt = self.prompt.saturating_add(usage.prompt);
        self.completion = self.completion.saturating_add(usage.completion);
        self.cached = self.cached.saturating_add(usage.cached.unwrap_or(0));
        self.cache_write = self
            .cache_write
            .saturating_add(usage.cache_write.unwrap_or(0));
        self.reasoning = self.reasoning.saturating_add(usage.reasoning.unwrap_or(0));
        self.calls += 1;
    }

    fn payload(&self) -> Value {
        json!({
            "prompt": self.prompt,
            "completion": self.completion,
            "cached": self.cached,
            "cache_write": self.cache_write,
            "reasoning": self.reasoning,
            "calls": self.calls,
        })
    }
}

/// The turn's usage so far.
#[derive(Default)]
struct Ledger {
    all: Totals,
    subagents: Totals,
    /// The turn's own model calls so far (the `usage` events' `step`).
    steps: u64,
}

/// The request occupancy the next `usage` event carries (#1217), and a
/// `context` event when its phase changes: the step compaction's estimate
/// of what the model call sends, never its content.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ContextMeter {
    /// The request's estimated input tokens.
    pub used_tokens: u64,
    /// The window the budget uses (the model's, at most 272k).
    pub window_tokens: u32,
    /// `measured`, `compacting` or `compacted`.
    pub phase: &'static str,
    /// The window is the desktop's default (128k): the catalogue did not
    /// name the model's.
    pub window_fallback: bool,
    /// When this turn last compacted (RFC 3339, UTC).
    pub compacted_at: Option<String>,
}

impl ContextMeter {
    /// The share of the window in use, 0 to 100 (the meter's figure).
    fn percent(&self) -> u64 {
        if self.window_tokens == 0 {
            return 0;
        }
        (self.used_tokens.saturating_mul(100) / u64::from(self.window_tokens)).min(100)
    }

    fn payload(&self) -> Value {
        let mut value = json!({
            "used_tokens": self.used_tokens,
            "window_tokens": self.window_tokens,
            "percent": self.percent(),
            "phase": self.phase,
            "compacted": self.compacted_at.is_some(),
            "window_fallback": self.window_fallback,
        });
        if let Some(at) = &self.compacted_at {
            value["compacted_at"] = json!(at);
        }
        value
    }
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
            context: Mutex::new(None),
            usage: Mutex::new(Ledger::default()),
        }
    }

    /// The occupancy of the model call about to be made; a `compacted`
    /// phase stamps the compaction time, which later steps keep. A phase
    /// other than the last one's (a turn starts `measured`) is sent at once
    /// as a `context` event, so the meter shows a summary call while it
    /// runs; the rest ride on the next `usage` event.
    pub fn context(&self, mut meter: ContextMeter) {
        let changed = {
            let mut slot = self
                .context
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            if meter.phase == "compacted" {
                meter.compacted_at =
                    Some(chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Millis, true));
            } else if meter.compacted_at.is_none() {
                meter.compacted_at = slot.as_ref().and_then(|last| last.compacted_at.clone());
            }
            let last = slot.as_ref().map_or("measured", |last| last.phase);
            let changed = (last != meter.phase).then(|| meter.payload());
            *slot = Some(meter);
            changed
        };
        if let Some(payload) = changed {
            self.send("context", payload);
        }
    }

    fn context_payload(&self) -> Option<Value> {
        self.context
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_ref()
            .map(ContextMeter::payload)
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
    /// (`project_instructions`, only when there are some) and the model it
    /// runs on (`model`: `{name, reasoning_effort?, chosen, pinned}`, #1228).
    pub fn running(&self, project_instructions: &[String], model: &Value) {
        let mut payload = json!({ "phase": Phase::Running.as_str(), "model": model });
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

    /// The model's reasoning as it streams (#1228), next to `text_delta`:
    /// shown folded as "Thinking", never logged.
    pub fn thinking_delta(&self, text: &str) {
        if !text.is_empty() {
            self.send("thinking_delta", json!({ "text": text }));
        }
    }

    /// A reasoning block closed (the model moved on to its answer or its
    /// tool calls, or its stream ended): `chars` is the block's length.
    pub fn thinking_end(&self, chars: usize) {
        self.send("thinking_end", json!({ "chars": chars }));
    }

    /// One model call's token usage, as the gateway reported it: counted
    /// toward the turn's total whoever made the call ([`UsageLane`]), and
    /// for the turn's own calls sent as a `usage` event once the call's
    /// stream ended: `prompt_tokens` (what the call sent: the context in
    /// use, cached tokens included), `completion_tokens` (what it produced),
    /// the cached, cache-write and reasoning parts when the gateway names
    /// them, the model, the call's `step` in the turn (from 1), and
    /// `context`, the call's request occupancy as the step compaction
    /// measured it (#1217): the composer's context meter.
    pub fn model_usage(&self, lane: UsageLane, model: &str, usage: CallUsage) {
        let step = {
            let mut ledger = self
                .usage
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            ledger.all.add(&usage);
            match lane {
                UsageLane::Turn => {
                    ledger.steps += 1;
                    ledger.steps
                }
                UsageLane::SubAgent => {
                    ledger.subagents.add(&usage);
                    return;
                }
                UsageLane::Summary => return,
            }
        };
        let mut payload = json!({
            "model": model,
            "step": step,
            "prompt_tokens": usage.prompt,
            "completion_tokens": usage.completion,
            "total_tokens": usage.total,
        });
        for (key, value) in [
            ("cached_tokens", usage.cached),
            ("cache_write_tokens", usage.cache_write),
            ("reasoning_tokens", usage.reasoning),
        ] {
            if let Some(value) = value.filter(|n| *n > 0) {
                payload[key] = json!(value);
            }
        }
        if let Some(context) = self.context_payload() {
            payload["context"] = context;
        }
        self.send("usage", payload);
    }

    /// `usage_total`, once the turn's run ended (before `done`): every model
    /// call made on the turn's execution id, the agent's steps, compaction
    /// summaries and sub-agents together (what the gateway's request log
    /// holds for it), with the sub-agents' share apart when there was any.
    pub fn usage_total(&self) {
        let (all, subagents) = {
            let ledger = self
                .usage
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            (ledger.all, ledger.subagents)
        };
        let mut payload = all.payload();
        if subagents.calls > 0 {
            payload["subagents"] = subagents.payload();
        }
        self.send("usage_total", payload);
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
    /// The app channel's process events (#1223).
    pub process_events: Mutex<Vec<super::processes::ProcessEvent>>,
}

#[cfg(test)]
impl VecEmitter {
    pub fn all(&self) -> Vec<AgentEvent> {
        self.events.lock().expect("lock").clone()
    }

    pub fn kinds(&self) -> Vec<&'static str> {
        self.all().iter().map(|event| event.kind).collect()
    }

    pub fn processes(&self) -> Vec<super::processes::ProcessEvent> {
        self.process_events.lock().expect("lock").clone()
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

    fn emit_process(&self, event: super::processes::ProcessEvent) {
        self.process_events.lock().expect("lock").push(event);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_context_phase_change_is_sent_at_once() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter.clone());
        let meter = |phase: &'static str, used_tokens: u64| ContextMeter {
            used_tokens,
            window_tokens: 8_192,
            phase,
            ..ContextMeter::default()
        };
        events.context(meter("measured", 100));
        events.context(meter("measured", 200));
        events.context(meter("compacting", 7_000));
        events.context(meter("compacted", 1_500));
        events.context(meter("compacted", 1_600));
        events.context(meter("measured", 1_700));
        events.model_usage(
            UsageLane::Turn,
            "m",
            CallUsage {
                prompt: 1,
                completion: 1,
                total: 2,
                ..CallUsage::default()
            },
        );
        let all = emitter.all();
        let sent: Vec<(&str, &str)> = all
            .iter()
            .map(|event| {
                let payload = if event.kind == "usage" {
                    &event.payload["context"]
                } else {
                    &event.payload
                };
                (event.kind, payload["phase"].as_str().unwrap())
            })
            .collect();
        assert_eq!(
            sent,
            [
                ("context", "compacting"),
                ("context", "compacted"),
                ("context", "measured"),
                ("usage", "measured"),
            ]
        );
        assert_eq!(all[0].payload["used_tokens"], 7_000);
        assert_eq!(all[0].payload["percent"], 85);
        assert_eq!(all[0].payload["compacted"], false);
        assert!(all[1].payload["compacted_at"].is_string());
        assert_eq!(all[2].payload["compacted"], true, "the compaction is kept");
    }

    #[test]
    fn a_status_carries_the_phases_timed_since_the_last_one() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter.clone());
        events.status(Phase::Resolving, None);
        events.timed(Timed::Resolving, Duration::from_millis(120));
        events.timed(Timed::Participant, Duration::from_millis(80));
        events.status(Phase::Starting, None);
        events.status(Phase::Starting, Some("again"));
        events.running(&[], &Value::Null);
        // Two overlapping tool calls, then a later one: their union.
        events.tool_began();
        events.tool_began();
        events.first_token();
        events.first_token();
        events.tool_ended();
        events.tool_ended();
        events.tools_done();
        events.status(Phase::Done, None);
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

    #[test]
    fn every_call_counts_toward_the_total_and_only_the_turns_own_send_usage() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter.clone());
        let call = |prompt: u64, cached: Option<u64>| CallUsage {
            prompt,
            completion: 10,
            total: prompt + 10,
            cached,
            cache_write: None,
            reasoning: Some(4),
        };
        events.model_usage(UsageLane::Turn, "m", call(1_000, None));
        events.model_usage(UsageLane::SubAgent, "m", call(300, Some(100)));
        events.model_usage(UsageLane::Summary, "m", call(500, None));
        events.model_usage(UsageLane::Turn, "m", call(1_200, Some(900)));
        events.usage_total();
        let all = emitter.all();
        assert_eq!(
            all.iter().map(|e| e.kind).collect::<Vec<_>>(),
            ["usage", "usage", "usage_total"]
        );
        assert_eq!(all[0].payload["step"], 1);
        assert!(all[0].payload.get("cached_tokens").is_none());
        assert_eq!(all[1].payload["step"], 2);
        assert_eq!(all[1].payload["cached_tokens"], 900);
        assert_eq!(all[1].payload["reasoning_tokens"], 4);
        assert_eq!(all[1].payload["model"], "m");
        assert_eq!(
            all[2].payload,
            json!({
                "prompt": 3_000, "completion": 40, "cached": 1_000, "cache_write": 0,
                "reasoning": 16, "calls": 4,
                "subagents": {"prompt": 300, "completion": 10, "cached": 100,
                    "cache_write": 0, "reasoning": 4, "calls": 1},
            })
        );
    }
}
