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
//!   sends ([`super::model::request_body_in`]), not an estimate of the adk
//!   contents.
//! * **Summary model.** The project's low-tier default model when the
//!   catalogue lists one whose described input window (its context window,
//!   or its `max_input_tokens` when smaller) is at least the agent's
//!   ([`ModelFacts::summary_model`]), else the agent's own model; either
//!   through the same `/llm` transport and execution id (spend is the
//!   turn's), bound quiet: the webview sees no text or usage of it. No
//!   prompt-cache directives are sent.
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
    ModelRequestBudget, RequestContextBudget, RequestContextUsage, estimate_tokens,
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
#[derive(Clone, Debug)]
pub struct ModelFacts {
    pub limits: ModelContextLimits,
    pub replay: ReasoningReplay,
    pub vision: bool,
    /// The project's low-tier default model, when the catalogue lists one.
    pub low_tier: Option<LowTierModel>,
}

/// A low-tier model a compaction may summarise with.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LowTierModel {
    pub name: String,
    pub limits: ModelContextLimits,
    pub replay: ReasoningReplay,
}

impl ModelFacts {
    /// The compaction's summary model other than the agent's own: the
    /// low-tier default when its window is described and its input window
    /// is at least the agent's `limits`' (the summary request is sized to
    /// what the agent's model takes in); `None`: the agent's own model.
    #[must_use]
    pub fn summary_model(&self, limits: &ModelContextLimits) -> Option<&LowTierModel> {
        self.low_tier.as_ref().filter(|model| {
            !model.limits.context_window_fallback
                && input_window(&model.limits) >= input_window(limits)
        })
    }
}

/// What a model takes in: its context window, or its `max_input_tokens`
/// when smaller.
fn input_window(limits: &ModelContextLimits) -> u32 {
    limits
        .max_input_tokens
        .map_or(limits.context_window_tokens, |input| {
            input.min(limits.context_window_tokens)
        })
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
            replay: known.as_ref().map(|facts| facts.replay).unwrap_or_default(),
            vision: known.as_ref().is_some_and(|facts| facts.vision),
            low_tier: known.and_then(|facts| facts.low_tier),
        };
    }
    let kept_already = kept.is_some();
    if let Some(facts) = from_hand.or(kept) {
        if let (Some(key), false) = (key, kept_already) {
            caches.keep_limits(key, facts.clone());
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
            low_tier: None,
        };
    };
    let facts = facts_of(&catalogue, model_name);
    if let Some(key) = key {
        caches.keep_limits(key, facts.clone());
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
        low_tier: catalogue.low_tier_default().and_then(|model| {
            Some(LowTierModel {
                name: model.name.clone(),
                limits: limits_of(&model.row)?,
                replay: ReasoningReplay::of(Some(model)),
            })
        }),
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

/// What fills a measured request, in estimated tokens: each part of the
/// provider body at [`estimate_tokens`]' rate (estimates, never content).
/// `message_tokens` takes the rest of the request's estimate (the history,
/// the host context and the body's own keys), so the parts add up to it.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct ContextParts {
    /// The system message: the agent's instructions, the environment and
    /// the project instructions.
    pub system_tokens: u64,
    /// The declarations of the host's and the agent's tools.
    pub tool_tokens: u64,
    /// The declarations of the person's local MCP tools (`mcp__`).
    pub mcp_tool_tokens: u64,
    /// The `load_skill` catalogue and the skills picked for the turn.
    pub skill_tokens: u64,
    /// The conversation: everything else.
    pub message_tokens: u64,
}

/// The parts of the last request [`ProviderBudget`] measured, for the
/// [`ContextHost`] that reports it.
pub type MeasuredParts = Arc<Mutex<Option<ContextParts>>>;

/// The tool that loads a skill from the agent's catalogue.
const LOAD_SKILL: &str = "load_skill";

fn encoded_len(value: &Value) -> usize {
    serde_json::to_vec(value).map_or(0, |bytes| bytes.len())
}

/// The bytes of `text` in the picked skills' section
/// ([`super::skills::SKILLS_HEADING`] to its end line).
fn skill_section_len(text: &str) -> usize {
    let Some(start) = text.find(super::skills::SKILLS_HEADING) else {
        return 0;
    };
    let end = text[start..]
        .find(super::skills::END_OF_SKILLS)
        .map_or(text.len(), |at| {
            start + at + super::skills::END_OF_SKILLS.len()
        });
    end - start
}

/// The parts of `body` (a provider body whose estimate is `estimated`).
#[must_use]
pub fn parts_of(body: &Value, estimated: u64) -> ContextParts {
    let mut system = 0usize;
    let mut skills = 0usize;
    let mut tools = 0usize;
    let mut mcp = 0usize;
    for (at, message) in body["messages"]
        .as_array()
        .into_iter()
        .flatten()
        .enumerate()
    {
        if at == 0 && message["role"] == "system" {
            system += encoded_len(message);
        } else if message["name"] == super::model::HOST_CONTEXT_NAME {
            skills += message["content"].as_str().map_or(0, skill_section_len);
        }
    }
    for tool in body["tools"].as_array().into_iter().flatten() {
        let name = tool["function"]["name"].as_str().unwrap_or_default();
        let len = encoded_len(tool);
        if name.starts_with(elitea_local_tools::mcp::naming::PREFIX) {
            mcp += len;
        } else if name == LOAD_SKILL {
            skills += len;
        } else {
            tools += len;
        }
    }
    let system_tokens = estimate_tokens(system);
    let tool_tokens = estimate_tokens(tools);
    let mcp_tool_tokens = estimate_tokens(mcp);
    let skill_tokens = estimate_tokens(skills);
    let named = system_tokens + tool_tokens + mcp_tool_tokens + skill_tokens;
    ContextParts {
        system_tokens,
        tool_tokens,
        mcp_tool_tokens,
        skill_tokens,
        message_tokens: estimated.saturating_sub(named),
    }
}

/// Measures the provider body [`super::model::GatewayModel`] sends.
pub struct ProviderBudget {
    pub model: ModelRequest,
    pub budget: RequestContextBudget,
    /// As the transport sends history (#1228).
    pub replay: ReasoningReplay,
    /// The turn's context message and cache breakpoints, as the transport
    /// lays them out (#1227): the tail weighs what it sends.
    pub tail: Option<Arc<super::model::TurnTail>>,
    pub cache_hints: bool,
    /// Where the parts of each measured request go.
    pub parts: MeasuredParts,
}

impl ModelRequestBudget for ProviderBudget {
    fn measure(&self, request: &LlmRequest) -> adk_core::Result<RequestContextUsage> {
        let body = super::model::request_body_in(
            &self.model,
            request,
            super::model::Layout {
                replay: self.replay,
                images: super::model::Images::Measure,
                tail: self.tail.as_deref(),
                cache_hints: self.cache_hints,
            },
        )?;
        let encoded = serde_json::to_vec(&body)
            .map_err(|_| adk_core::AdkError::model("the model request could not be encoded"))?;
        let usage = self
            .budget
            .measure_provider_request(&encoded, MAX_MODEL_REQUEST_BYTES)?;
        *self
            .parts
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) =
            Some(parts_of(&body, usage.estimated_input));
        Ok(usage)
    }
}

/// What the turn keeps of its compaction (the commit's summary), and
/// whether its message went to the model.
#[derive(Default)]
pub struct TurnContext {
    compaction: Mutex<Option<Arc<StepCompaction>>>,
    ran: std::sync::atomic::AtomicBool,
}

impl TurnContext {
    /// The person's message passed its `UserPromptSubmit` hooks and the
    /// run starts: the turn's `Stop` hooks run when it ends.
    pub fn mark_ran(&self) {
        self.ran.store(true, std::sync::atomic::Ordering::Relaxed);
    }

    #[must_use]
    pub fn ran(&self) -> bool {
        self.ran.load(std::sync::atomic::Ordering::Relaxed)
    }

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
    /// The parts of the request just measured ([`ProviderBudget::parts`]).
    pub parts: MeasuredParts,
    /// The estimate at which the step compaction starts
    /// ([`CompactionPolicy::trigger_tokens`]).
    pub compact_at: u64,
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
            measured_at: None,
            compact_at_tokens: self.compact_at,
            parts: *self
                .parts
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
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
    fn a_request_is_split_into_its_parts_and_the_parts_add_up() {
        let tail = format!(
            "Today is Monday.\n{}\nUse the style.\n{}",
            crate::d0::skills::SKILLS_HEADING,
            crate::d0::skills::END_OF_SKILLS
        );
        let body = json!({
            "model": "m",
            "messages": [
                {"role": "system", "content": "Be brief.".repeat(40)},
                {"role": "user", "content": "earlier question"},
                {"role": "user", "name": crate::d0::model::HOST_CONTEXT_NAME, "content": tail},
                {"role": "user", "content": "now"},
            ],
            "tools": [
                {"type": "function", "function": {"name": "read_file", "description": "x".repeat(80)}},
                {"type": "function", "function": {"name": "mcp__fx__echo", "description": "y".repeat(40)}},
                {"type": "function", "function": {"name": "load_skill", "description": "z".repeat(20)}},
            ],
        });
        let estimated = estimate_tokens(serde_json::to_vec(&body).unwrap().len());
        let parts = parts_of(&body, estimated);
        assert!(parts.system_tokens > parts.tool_tokens, "{parts:?}");
        assert!(parts.tool_tokens > parts.mcp_tool_tokens, "{parts:?}");
        assert!(
            parts.mcp_tool_tokens > 0 && parts.skill_tokens > 0,
            "{parts:?}"
        );
        assert!(parts.message_tokens > 0, "{parts:?}");
        assert_eq!(
            parts.system_tokens
                + parts.tool_tokens
                + parts.mcp_tool_tokens
                + parts.skill_tokens
                + parts.message_tokens,
            estimated
        );
        // A tail without picked skills counts as messages only.
        assert_eq!(skill_section_len("Today is Monday."), 0);
        // Parts never pass a smaller estimate.
        assert_eq!(parts_of(&body, 1).message_tokens, 0);
    }

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
    fn summaries_go_to_the_low_tier_model_only_when_its_window_is_as_large() {
        let catalogue = |low_window: Value| {
            Catalogue::from_body(
                &json!({
                    "items": [
                        {"name": "agent", "project_id": 1, "context_window": 200_000, "max_output_tokens": 32_000},
                        {"name": "small", "project_id": 1, "context_window": low_window,
                         "max_output_tokens": 8_000, "openai_compatible": false},
                    ],
                    "low_tier_default_model_name": "small",
                    "low_tier_default_model_project_id": 1,
                }),
                1,
            )
        };
        let facts = facts_of(&catalogue(json!(200_000)), "agent");
        let low = facts.summary_model(&facts.limits).unwrap();
        assert_eq!(low.name, "small");
        assert_eq!(low.limits.max_output_tokens, 8_000);
        assert_eq!(low.replay, ReasoningReplay::SignedBlocks);
        // A smaller window, or one the catalogue does not describe: the
        // agent's own model.
        let smaller = facts_of(&catalogue(json!(128_000)), "agent");
        assert!(smaller.low_tier.is_some());
        assert!(smaller.summary_model(&smaller.limits).is_none());
        let undescribed = facts_of(&catalogue(json!(null)), "agent");
        assert!(undescribed.summary_model(&undescribed.limits).is_none());
        // Measured against the limits the turn runs on (frozen ones too).
        let frozen = ModelContextLimits {
            context_window_tokens: 100_000,
            ..facts.limits
        };
        assert!(smaller.summary_model(&frozen).is_some());
        // The same window but a smaller input limit is too small; the
        // agent's own input limit counts the same way.
        let capped = Catalogue::from_body(
            &json!({
                "items": [
                    {"name": "agent", "project_id": 1, "context_window": 200_000, "max_output_tokens": 32_000},
                    {"name": "small", "project_id": 1, "context_window": 200_000,
                     "max_output_tokens": 8_000, "max_input_tokens": 64_000},
                ],
                "low_tier_default_model_name": "small",
                "low_tier_default_model_project_id": 1,
            }),
            1,
        );
        let capped = facts_of(&capped, "agent");
        assert!(capped.summary_model(&capped.limits).is_none());
        let agent_input = ModelContextLimits {
            max_input_tokens: Some(64_000),
            ..capped.limits
        };
        assert!(capped.summary_model(&agent_input).is_some());
        // No low-tier default configured.
        let none = facts_of(&Catalogue::default(), "agent");
        assert!(none.summary_model(&FALLBACK_LIMITS).is_none());
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
