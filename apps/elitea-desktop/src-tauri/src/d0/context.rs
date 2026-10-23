//! A local turn's context window (#1217): the model's limits, the step
//! compaction that keeps a long session inside them, and the marker that
//! lets the next turn start from the summary.
//!
//! * **Limits.** The resolved version's frozen `model_context_limits` when
//!   it carries them, else the model catalogue of the turn's project
//!   (`GET /configurations/models/{project}?section=llm&include_shared=true`),
//!   kept for the app's session ([`TurnCaches`]) once it answered for
//!   good (a 2xx, or a 404); a model the catalogue does not describe runs
//!   on [`FALLBACK_LIMITS`] (the runtime's 128k elision window, 4k output,
//!   marked as a fallback). A catalogue that could not answer now (a
//!   network error, 401, 403, 408, 429, a 5xx) is asked again next turn.
//! * **Order.** Older tool results are elided first (past 60% of the window,
//!   `elitea_agent_runtime::context`, a `before_model` callback); the step
//!   compaction wraps the model, so it measures the request the elision
//!   left and compacts only when that is still past its trigger.
//! * **Budget.** [`ProviderBudget`] measures the body the gateway transport
//!   sends ([`super::model::request_body`]), not an estimate of the adk
//!   contents.
//! * **Summary model.** The agent's own model, through the same `/llm`
//!   transport and execution id (spend is the turn's), bound quiet: the
//!   webview sees no text or usage of it. No prompt-cache directives are
//!   sent.
//! * **Marker.** A compaction appends an adk `EventCompaction` to the
//!   turn's session ([`ContextHost`]), placed after the last event it
//!   covers: the next turn's history starts from the summary instead of
//!   summarising the thread again. The commit carries the latest summary
//!   (`context_summary`, the shared v1 record), for the cloud executor.
//! * **Meter.** Each model call's occupancy goes to the next `usage`
//!   event ([`super::events::ContextMeter`]), and to a `context` event of
//!   its own when its phase changes (a compaction starts or ends):
//!   estimates, never content.

use std::sync::{Arc, Mutex};

use adk_core::{Content, Event, EventCompaction, LlmRequest, Part};
use adk_session::{GetRequest, SessionService};
use async_trait::async_trait;
use elitea_agent_runtime::context_budget::{
    ModelRequestBudget, RequestContextBudget, RequestContextUsage,
};
use elitea_agent_runtime::context_management::{
    Compaction, CompactionHost, CompactionPolicy, FileOp, FileTouch, SESSION_COMPACTION_KEY,
    StepCompaction, history_digest, visible_events,
};
use elitea_agent_runtime::context_status::{ContextPhase, ModelContextStatus};
use elitea_agent_runtime::host::ModelRequest;
use elitea_agent_runtime::request::ModelContextLimits;
use serde_json::{Map, Value};

use super::api::PlatformApi;
use super::cache::{LimitsKey, TurnCaches};
use super::catalogue::{self, Catalogue, ReasoningReplay};
use super::events::{ContextMeter, TurnEvents};
use super::recorder::{Recorder, ToolCalls};
use super::session::AGENT_AUTHOR;

/// The limits of a model the catalogue does not describe: the runtime's
/// elision window (`elitea_agent_runtime::context::FALLBACK_WINDOW_TOKENS`),
/// so a large model missing from the catalogue keeps the room it had.
pub const FALLBACK_LIMITS: ModelContextLimits = ModelContextLimits {
    context_window_tokens: 128_000,
    max_output_tokens: 4_096,
    context_window_fallback: true,
    max_output_fallback: true,
    max_input_tokens: None,
};

/// The largest model request body the budget lets through (the worker's).
pub const MAX_MODEL_REQUEST_BYTES: usize = 8 * 1024 * 1024;

/// The author of a compaction marker (adk skips markers whatever their
/// author; this one names who wrote it).
pub const MARKER_AUTHOR: &str = "elitea_context_compaction";

/// What a turn knows of its model from the catalogue: its context limits,
/// whether its reasoning goes back to the provider (#1228) and whether it
/// sees images (#1229; `false` when the catalogue was not read for it).
#[derive(Clone, Copy, Debug)]
pub struct ModelFacts {
    pub limits: ModelContextLimits,
    pub replay: ReasoningReplay,
    pub vision: bool,
}

/// The facts of the turn's model (module docs), with at most one catalogue
/// read: `in_hand` is the catalogue the turn already read (to check a chosen
/// model), else the kept facts, else one read. Frozen limits read nothing:
/// the family then comes from `in_hand` or the kept facts, else is
/// [`ReasoningReplay::Never`] (no read for it alone). Never fails: a
/// catalogue that cannot be read is the fallback, not a refused turn.
pub async fn model_facts(
    api: &PlatformApi,
    caches: &TurnCaches,
    project_id: i64,
    model_name: &str,
    details: &Map<String, Value>,
    in_hand: Option<&Catalogue>,
) -> ModelFacts {
    let key = api.credentials().identity().map(|identity| LimitsKey {
        identity,
        project_id,
        model_name: model_name.to_owned(),
    });
    let kept = key.as_ref().and_then(|key| caches.limits(key));
    let from_hand = in_hand.map(|catalogue| facts_of(catalogue, model_name));
    if let Some(frozen) = details
        .get("model_context_limits")
        .and_then(|value| serde_json::from_value::<ModelContextLimits>(value.clone()).ok())
        .filter(valid)
    {
        let known = from_hand.or(kept);
        return ModelFacts {
            limits: frozen,
            replay: known.map(|facts| facts.replay).unwrap_or_default(),
            vision: known.is_some_and(|facts| facts.vision),
        };
    }
    if let Some(facts) = from_hand.or(kept) {
        if let (Some(key), None) = (key, kept) {
            caches.keep_limits(key, facts);
        }
        return facts;
    }
    // A definitive answer is kept, whatever it said (a model it does not
    // describe stays on the fallback for the session); a transient one is
    // asked again next turn.
    let Some(catalogue) = catalogue::read(api, &caches.catalogues, project_id).await else {
        return ModelFacts {
            limits: FALLBACK_LIMITS,
            replay: ReasoningReplay::Never,
            vision: false,
        };
    };
    let facts = facts_of(&catalogue, model_name);
    if let Some(key) = key {
        caches.keep_limits(key, facts);
    }
    facts
}

/// `model_name`'s facts in `catalogue` (the project's own model wins over a
/// shared one; one it does not describe: the fallback).
fn facts_of(catalogue: &Catalogue, model_name: &str) -> ModelFacts {
    let model = catalogue.find(model_name);
    ModelFacts {
        limits: model
            .and_then(|model| limits_of(&model.row))
            .unwrap_or(FALLBACK_LIMITS),
        replay: ReasoningReplay::of(model),
        vision: model
            .and_then(|model| model.row.get("supports_vision"))
            .and_then(Value::as_bool)
            .unwrap_or(false),
    }
}

/// A catalogue row's limits (`context_window`, `max_output_tokens`,
/// `max_input_tokens`); an absent one is the fallback's, and marked so.
fn limits_of(item: &Value) -> Option<ModelContextLimits> {
    let read = |key: &str| -> Result<Option<u32>, ()> {
        match item.get(key) {
            None | Some(Value::Null) => Ok(None),
            Some(value) => value
                .as_u64()
                .and_then(|value| u32::try_from(value).ok())
                .filter(|value| *value > 0)
                .map(Some)
                .ok_or(()),
        }
    };
    let window = read("context_window").ok()?;
    let output = read("max_output_tokens").ok()?;
    let input = read("max_input_tokens").ok()?;
    let limits = ModelContextLimits {
        context_window_tokens: window.unwrap_or(FALLBACK_LIMITS.context_window_tokens),
        max_output_tokens: output.unwrap_or(FALLBACK_LIMITS.max_output_tokens),
        context_window_fallback: window.is_none(),
        max_output_fallback: output.is_none(),
        max_input_tokens: input,
    };
    valid(&limits).then_some(limits)
}

fn valid(limits: &ModelContextLimits) -> bool {
    limits.context_window_tokens > 0
        && limits.max_output_tokens > 0
        && limits.max_output_tokens < limits.context_window_tokens
        && limits.max_input_tokens != Some(0)
}

/// The compaction policy of the agent version: its `context_settings`
/// when it carries them (`enabled: false` or `enable_summarization: false`
/// turn it off, `session_compaction` tunes it, `summary_instructions`
/// guide the summary), else the defaults (pi's 16k reserve, 20k kept).
#[must_use]
pub fn policy(details: &Map<String, Value>) -> Option<CompactionPolicy> {
    let Some(settings) = details.get("context_settings").and_then(Value::as_object) else {
        return Some(CompactionPolicy::default());
    };
    let off = |key: &str| settings.get(key) == Some(&Value::Bool(false));
    if off("enabled") || off("enable_summarization") || off(SESSION_COMPACTION_KEY) {
        return None;
    }
    let policy = match CompactionPolicy::admit(settings) {
        Ok(Some(policy)) => policy,
        Ok(None) => CompactionPolicy::default(),
        Err(_) => {
            log::warn!("the agent's session_compaction setting is malformed; using the defaults");
            CompactionPolicy::default()
        }
    };
    match settings
        .get("summary_instructions")
        .and_then(Value::as_str)
        .filter(|text| !text.trim().is_empty())
    {
        Some(guidance) => Some(policy.with_guidance(guidance)),
        None => Some(policy),
    }
}

/// Measures the provider body [`super::model::GatewayModel`] sends.
pub struct ProviderBudget {
    pub model: ModelRequest,
    pub budget: RequestContextBudget,
    /// As the transport sends history (#1228).
    pub replay: ReasoningReplay,
}

impl ModelRequestBudget for ProviderBudget {
    fn measure(&self, request: &LlmRequest) -> adk_core::Result<RequestContextUsage> {
        let body = super::model::request_body(
            &self.model,
            request,
            self.replay,
            super::model::Images::Measure,
        )?;
        let encoded = serde_json::to_vec(&body)
            .map_err(|_| adk_core::AdkError::model("the model request could not be encoded"))?;
        self.budget
            .measure_provider_request(&encoded, MAX_MODEL_REQUEST_BYTES)
    }
}

/// What the turn keeps of its compaction: the commit's summary.
#[derive(Default)]
pub struct TurnContext {
    compaction: Mutex<Option<Arc<StepCompaction>>>,
}

impl TurnContext {
    pub fn bind(&self, compaction: Arc<StepCompaction>) {
        *self
            .compaction
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(compaction);
    }

    /// The latest v1 summary the turn's requests carried (a stored marker's
    /// or one this turn made), as the commit's `context_summary`.
    #[must_use]
    pub fn take_summary(&self) -> Option<Value> {
        let compaction = self
            .compaction
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take()?;
        let summary = compaction.latest_summary()?;
        serde_json::from_str(&summary).ok()
    }
}

/// The desktop's [`CompactionHost`].
pub struct ContextHost {
    pub events: Arc<TurnEvents>,
    pub recorder: Arc<Recorder>,
    pub service: Arc<dyn SessionService>,
    pub session_id: String,
    pub limits: ModelContextLimits,
}

#[async_trait]
impl CompactionHost for ContextHost {
    fn files_touched(&self, folded: &[Content]) -> Vec<FileTouch> {
        let mut touched = files_called(folded);
        touched.extend(files_touched(&self.recorder.tool_calls()));
        touched
    }

    fn status(&self, status: &ModelContextStatus) {
        self.events.context(ContextMeter {
            used_tokens: status.estimated_input_tokens(),
            window_tokens: status.total_tokens(),
            phase: match status.phase {
                ContextPhase::Measured => "measured",
                ContextPhase::Compacting => "compacting",
                ContextPhase::Compacted => "compacted",
            },
            window_fallback: self.limits.context_window_fallback,
            compacted_at: None,
        });
    }

    async fn compacted(&self, compaction: &Compaction) {
        match self.place_marker(compaction).await {
            Ok(()) => log::info!(
                "a local turn compacted its context ({} events folded{})",
                compaction.covered_events,
                if compaction.forced { ", asked for" } else { "" }
            ),
            Err(reason) => log::info!(
                "a local turn compacted its context but kept no marker: {reason} (the next turn summarises again)"
            ),
        }
    }
}

impl ContextHost {
    /// Append the marker after the last event the summary covers, when the
    /// session's events are the ones the request was built from.
    async fn place_marker(&self, compaction: &Compaction) -> Result<(), &'static str> {
        let session = self
            .service
            .get(GetRequest {
                app_name: crate::sessions::APP_NAME.to_owned(),
                user_id: crate::sessions::ADK_USER.to_owned(),
                session_id: self.session_id.clone(),
                num_recent_events: None,
                after: None,
            })
            .await
            .map_err(|_| "the session could not be read")?;
        let events = session.events().all();
        let marker = marker_for(&events, compaction)?;
        self.service
            .append_event(&self.session_id, marker)
            .await
            .map_err(|_| "the session refused the marker")
    }
}

/// The marker event for `compaction` over `events` (module docs).
///
/// # Errors
///
/// The events are not the ones the request was built from (another digest),
/// or an event after the covered ones shares the last one's timestamp (adk
/// would skip it too).
pub fn marker_for(events: &[Event], compaction: &Compaction) -> Result<Event, &'static str> {
    let visible = visible_events(events, AGENT_AUTHOR, compaction.based_on.as_ref());
    let covered = compaction.covered_events;
    if covered == 0 || covered > visible.len() {
        return Err("the session holds fewer events than the summary covers");
    }
    let contents: Vec<Content> = visible[..covered]
        .iter()
        .map(|(_, content)| content.clone())
        .collect();
    if history_digest(&contents) != compaction.events_digest {
        return Err("the session's events are not the request's history");
    }
    let start = visible[0].0.timestamp;
    let end = visible[covered - 1].0.timestamp;
    if visible[covered..]
        .iter()
        .any(|(event, _)| event.timestamp <= end)
    {
        return Err("a kept event is not later than the folded ones");
    }
    let invocation = events
        .last()
        .map_or_else(String::new, |event| event.invocation_id.clone());
    let mut marker = Event::new(&invocation);
    MARKER_AUTHOR.clone_into(&mut marker.author);
    marker.actions.compaction = Some(EventCompaction {
        start_timestamp: start,
        end_timestamp: end,
        compacted_content: compaction.summary_content.clone(),
    });
    Ok(marker)
}

/// What the turn's tool calls did to files, for the summary's
/// `files_touched` entry: reads, writes, edits and a patch's deletes. A
/// call that failed touched nothing.
#[must_use]
pub fn files_touched(calls: &ToolCalls) -> Vec<FileTouch> {
    let mut touched = Vec::new();
    for (_, entry) in &calls.0 {
        if entry.get("finish_reason").and_then(Value::as_str) == Some("error") {
            continue;
        }
        if let Some(name) = entry.get("tool_name").and_then(Value::as_str) {
            touches_of(
                name,
                entry.get("tool_inputs").unwrap_or(&Value::Null),
                &mut touched,
            );
        }
    }
    touched
}

/// What the calls in `contents` (history about to be folded, earlier
/// turns' included) did to files.
#[must_use]
pub fn files_called(contents: &[Content]) -> Vec<FileTouch> {
    let mut touched = Vec::new();
    for part in contents.iter().flat_map(|content| &content.parts) {
        if let Part::FunctionCall { name, args, .. } = part {
            touches_of(name, args, &mut touched);
        }
    }
    touched
}

/// The files one local tool call names, by its tool (the local tools'
/// names and schemas, `elitea_local_tools::session::TOOLS`).
fn touches_of(name: &str, inputs: &Value, touched: &mut Vec<FileTouch>) {
    let mut push = |path: &str, op: FileOp| {
        if !path.is_empty() {
            touched.push(FileTouch {
                path: path.to_owned(),
                op,
            });
        }
    };
    let path = inputs
        .get("path")
        .and_then(Value::as_str)
        .unwrap_or_default();
    match name {
        "read_file" | "read_document" => push(path, FileOp::Read),
        "write_file" => push(path, FileOp::Write),
        "edit_file" => push(path, FileOp::Edit),
        "apply_patch" => {
            let Some(patch) = inputs.get("patch").and_then(Value::as_str) else {
                return;
            };
            let Ok(files) = elitea_local_tools::patch::parse(patch) else {
                return;
            };
            for file in &files {
                match (&file.old_path, &file.new_path) {
                    (None, Some(new)) => push(new, FileOp::Write),
                    (Some(old), None) => push(old, FileOp::Delete),
                    (Some(old), Some(new)) if file.is_rename() => {
                        push(old, FileOp::Delete);
                        push(new, FileOp::Write);
                    }
                    (Some(_), Some(new)) => push(new, FileOp::Edit),
                    (None, None) => {}
                }
            }
        }
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn catalogue_rows_give_limits_and_mark_what_they_lack() {
        let full = limits_of(&json!({"name": "m", "context_window": 200_000,
            "max_output_tokens": 32_000, "max_input_tokens": 180_000}))
        .unwrap();
        assert_eq!(full.context_window_tokens, 200_000);
        assert_eq!(full.max_input_tokens, Some(180_000));
        assert!(!full.context_window_fallback && !full.max_output_fallback);
        let bare = limits_of(&json!({"name": "m"})).unwrap();
        assert_eq!(bare.context_window_tokens, 128_000);
        assert_eq!(bare.max_output_tokens, 4_096);
        assert!(bare.context_window_fallback && bare.max_output_fallback);
        // The window of a row that names only its output is the fallback's.
        let output_only = limits_of(&json!({"max_output_tokens": 64_000})).unwrap();
        assert_eq!(output_only.context_window_tokens, 128_000);
        assert!(output_only.context_window_fallback && !output_only.max_output_fallback);
        assert!(limits_of(&json!({"context_window": -1})).is_none());
        assert!(limits_of(&json!({"context_window": 1000, "max_output_tokens": 1000})).is_none());
    }

    #[test]
    fn the_agents_settings_switch_compaction_off_or_tune_it() {
        assert_eq!(policy(&Map::new()), Some(CompactionPolicy::default()));
        let details = |settings: Value| {
            let mut details = Map::new();
            details.insert("context_settings".into(), settings);
            details
        };
        assert_eq!(policy(&details(json!({"enabled": false}))), None);
        assert_eq!(policy(&details(json!({"session_compaction": false}))), None);
        let tuned = policy(&details(json!({
            "session_compaction": {"keep_recent_tokens": 5000},
            "summary_instructions": "Keep ticket ids."
        })))
        .unwrap();
        assert_eq!(tuned.keep_recent_tokens, 5000);
        assert_eq!(tuned.guidance, "Keep ticket ids.");
    }

    #[test]
    fn tool_calls_name_the_files_they_touched() {
        let entry = |name: &str, inputs: Value, failed: bool| {
            let mut map = Map::new();
            map.insert("tool_name".into(), json!(name));
            map.insert("tool_inputs".into(), inputs);
            map.insert(
                "finish_reason".into(),
                json!(if failed { "error" } else { "stop" }),
            );
            ("id".to_owned(), map)
        };
        let patch = "--- a/old.rs\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n";
        let calls = ToolCalls(vec![
            entry("read_file", json!({"path": "a.rs"}), false),
            entry("write_file", json!({"path": "b.rs", "content": ""}), false),
            entry("edit_file", json!({"path": "c.rs"}), true),
            entry("apply_patch", json!({"patch": patch}), false),
            entry("run_command", json!({"command": "ls"}), false),
        ]);
        let touched: Vec<(String, FileOp)> = files_touched(&calls)
            .into_iter()
            .map(|touch| (touch.path, touch.op))
            .collect();
        assert_eq!(
            touched,
            vec![
                ("a.rs".to_owned(), FileOp::Read),
                ("b.rs".to_owned(), FileOp::Write),
                ("old.rs".to_owned(), FileOp::Delete),
            ]
        );
    }
}
