//! Explicit ownership seam for conversation context management.
//!
//! Main freezes one settings contract per conversation and Rust admits it here,
//! before claim-scoped credentials are redeemed. Admission is the only place
//! that reads `context_settings`: anything the contract does not describe —
//! unknown keys, wrong types, out-of-range numbers, or a strategy this runtime
//! has no primitive for — is refused rather than silently degraded.
//!
//! # Composition
//!
//! Ordinary roots with frozen model limits use `context_compaction` through
//! the model checkpoint callback. That path stores structured summary coverage
//! and the prepared request before dispatch. Child and pipeline integration
//! remains separate work.
//!
//! Legacy bindings without frozen limits compose the plan onto the ADK Runner from
//! [`ContextManagementPlan::prepare_runner_composition`], which yields an
//! `adk_runner::compaction::CompactionConfig`. The config pairs
//! [`EliteaContextCompaction`] — a [`CompactionStrategy`] that reproduces the
//! current SDK's `SummarizationMiddleware.before_model` ordering — with
//! ADK-Rust 2.2.0's `LlmEventSummarizer`, which performs the actual summary
//! call.
//!
//! # Transcript lineage
//!
//! The ADK Runner applies a `CompactionStrategy` through
//! `MutableSession::replace_events`, which rewrites only the in-memory event
//! view for the current invocation. Nothing is appended to, or removed from,
//! the `SessionService` behind it, so the `PostgreSQL` checkpointer lineage
//! stays the single durable transcript and a resume always reloads the full
//! history and recompacts it. ADK's other compaction surface,
//! `EventsCompactionConfig`, persists `EventCompaction` markers while retaining
//! original events. Runner history uses the marker boundary on later calls.
//! This legacy strategy does not use that durable path.
//! See `docs/context-continuation-design.md` for the required integration.
//!
//! # Legacy strategy limits
//!
//! * The summary is derived per invocation and is never persisted, so an
//!   over-budget conversation pays one summarization call per turn.
//! * `apply_compaction_with_retry` only accepts a compacted list that fits the
//!   budget. When the untouchable tail alone exceeds `max_context_tokens` the
//!   Runner logs a warning and proceeds with the full, uncompacted history —
//!   today's behavior, never a truncated one.

use std::sync::Arc;

use adk_runner::compaction::{CompactionConfig, CompactionStrategy, estimate_event_tokens};
use adk_rust::agent::LlmEventSummarizer;
use adk_rust::{AdkError, BaseEventsSummarizer, Content, Event, Llm, Part};
use async_trait::async_trait;
use serde_json::{Map, Value};

use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

/// The prefix the current SDK puts in front of a rolled-up conversation.
const SUMMARY_PREFIX: &str = "Here is a summary of the conversation to date:";

/// `LlmEventSummarizer`'s required placeholder in a prompt template.
const ADK_HISTORY_PLACEHOLDER: &str = "{conversation_history}";

/// The placeholder the current SDK's `summary_instructions` may carry.
const SDK_HISTORY_PLACEHOLDER: &str = "{messages}";

/// Only one summarization pass per invocation, matching the current SDK.
const COMPACTION_ATTEMPTS: usize = 1;

/// Every key Main is allowed to freeze into `context_settings`.
const ADMITTED_KEYS: [&str; 9] = [
    "enabled",
    "budget_mode",
    "enable_summarization",
    "enable_context_editing",
    "max_context_tokens",
    "preserve_recent_messages",
    "preserve_system_messages",
    "summary_instructions",
    "summary_llm_settings",
];

const DEFAULT_MAX_CONTEXT_TOKENS: u64 = 64_000;
const MIN_MAX_CONTEXT_TOKENS: u64 = 1_000;
const DEFAULT_PRESERVE_RECENT_MESSAGES: u64 = 5;
const MIN_PRESERVE_RECENT_MESSAGES: u64 = 1;
const MAX_PRESERVE_RECENT_MESSAGES: u64 = 99;
const DEFAULT_SUMMARY_INSTRUCTIONS: &str =
    "Generate a concise summary of the following conversation messages";

/// Context behavior admitted before claim-scoped credentials are redeemed.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum ContextManagementPlan {
    /// No compaction or context editing is requested for this invocation.
    ///
    /// This also covers a conversation whose master switch is on but whose
    /// every strategy switch is off: the current SDK still installs its
    /// middleware for analytics, but the middleware returns before it changes
    /// anything the model sees, so the inference-time behavior is identical.
    Disabled,
    /// Summarize the oldest events once the budget is exceeded.
    Summarize(ContextCompactionPlan),
}

/// The frozen settings that drive one summarization pass.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ContextCompactionPlan {
    pub(super) max_context_tokens: u64,
    pub(super) preserve_recent_messages: usize,
    pub(super) preserve_system_messages: bool,
    pub(super) summary_instructions: String,
}

impl ContextManagementPlan {
    /// Interpret Main's frozen contract, refusing everything it does not
    /// describe. `settings` is the conversation's `context_settings` map and
    /// `conversation_id` scopes it — context management is per-conversation, so
    /// an absent conversation cannot carry a plan.
    ///
    /// # Errors
    ///
    /// Returns [`NativeAgentAssemblyErrorCode::InvalidInput`] for a malformed
    /// or out-of-range setting, and
    /// [`NativeAgentAssemblyErrorCode::UnsupportedCapability`] for a strategy
    /// this runtime has no primitive for.
    pub(crate) fn admit_current(
        settings: &Map<String, Value>,
        conversation_id: Option<&str>,
    ) -> Result<Self, NativeAgentAssemblyError> {
        if settings.is_empty() || conversation_id.is_none() {
            return Ok(Self::Disabled);
        }
        if let Some(key) = settings
            .keys()
            .find(|key| !ADMITTED_KEYS.contains(&key.as_str()))
        {
            tracing::debug!(
                setting = key.as_str(),
                "refused an unrecognized context management setting"
            );
            return Err(invalid_input());
        }
        if !admit_bool(settings, "enabled", true)? {
            return Ok(Self::Disabled);
        }
        // Clearing older tool outputs has no ADK-Rust 2.0.0 primitive. Admitting
        // it would silently drop half of what the setting promises.
        if admit_bool(settings, "enable_context_editing", false)? {
            return Err(unsupported());
        }
        // A dedicated summarization model needs a second credential resolution
        // that the claim does not carry.
        match settings.get("summary_llm_settings") {
            None | Some(Value::Null) => {}
            Some(Value::Object(_)) => return Err(unsupported()),
            Some(_) => return Err(invalid_input()),
        }
        let preserve_system_messages = admit_bool(settings, "preserve_system_messages", true)?;
        let max_context_tokens = admit_u64(
            settings,
            "max_context_tokens",
            DEFAULT_MAX_CONTEXT_TOKENS,
            MIN_MAX_CONTEXT_TOKENS,
            u64::from(u32::MAX),
        )?;
        let preserve_recent_messages = admit_u64(
            settings,
            "preserve_recent_messages",
            DEFAULT_PRESERVE_RECENT_MESSAGES,
            MIN_PRESERVE_RECENT_MESSAGES,
            MAX_PRESERVE_RECENT_MESSAGES,
        )?;
        let summary_instructions = match settings.get("summary_instructions") {
            Some(Value::String(value)) if !value.trim().is_empty() => value.clone(),
            None | Some(Value::Null | Value::String(_)) => DEFAULT_SUMMARY_INSTRUCTIONS.to_owned(),
            Some(_) => return Err(invalid_input()),
        };
        if !admit_bool(settings, "enable_summarization", true)? {
            return Ok(Self::Disabled);
        }
        let preserve_recent_messages =
            usize::try_from(preserve_recent_messages).map_err(|_| invalid_input())?;
        Ok(Self::Summarize(ContextCompactionPlan {
            max_context_tokens,
            preserve_recent_messages,
            preserve_system_messages,
            summary_instructions,
        }))
    }

    /// Compose the admitted plan onto the exclusive Runner, after admission and
    /// before the Runner is built. `summarization_model` is the invocation's
    /// isolated summary model; a caller that has none (the pipeline graph runs one model
    /// per node) passes `None` and an active plan is refused rather than
    /// half-applied.
    ///
    /// # Errors
    ///
    /// Returns [`NativeAgentAssemblyErrorCode::UnsupportedCapability`] when an
    /// active plan has no model to summarize with.
    pub(crate) fn prepare_runner_composition(
        self,
        summarization_model: Option<Arc<dyn Llm>>,
    ) -> Result<Option<CompactionConfig>, NativeAgentAssemblyError> {
        let Self::Summarize(plan) = self else {
            return Ok(None);
        };
        let Some(model) = summarization_model else {
            return Err(unsupported());
        };
        let budget = usize::try_from(plan.max_context_tokens).map_err(|_| invalid_input())?;
        let summarizer =
            LlmEventSummarizer::new(model).with_prompt_template(plan.prompt_template());
        let strategy = plan.strategy(Arc::new(summarizer));
        Ok(Some(CompactionConfig {
            strategy: Box::new(strategy),
            context_budget: budget,
            max_retries: COMPACTION_ATTEMPTS,
        }))
    }
}

impl ContextCompactionPlan {
    /// Translate the frozen `summary_instructions` into the placeholder
    /// `LlmEventSummarizer` expects, reproducing the current SDK's two shapes:
    /// a template that names its own placeholder, or a bare instruction that
    /// gets the transcript appended in a `<messages>` block.
    /// Build the strategy this plan drives. The Runner composition and the
    /// behavioral tests share this one seam.
    pub(super) fn strategy(
        &self,
        summarizer: Arc<dyn BaseEventsSummarizer>,
    ) -> EliteaContextCompaction {
        EliteaContextCompaction {
            summarizer,
            preserve_recent_messages: self.preserve_recent_messages,
            preserve_system_messages: self.preserve_system_messages,
        }
    }

    pub(super) fn prompt_template(&self) -> String {
        if self.summary_instructions.contains(SDK_HISTORY_PLACEHOLDER) {
            return self
                .summary_instructions
                .replace(SDK_HISTORY_PLACEHOLDER, ADK_HISTORY_PLACEHOLDER);
        }
        if self.summary_instructions.contains(ADK_HISTORY_PLACEHOLDER) {
            return self.summary_instructions.clone();
        }
        format!(
            "{}\n\n<messages>\n{ADK_HISTORY_PLACEHOLDER}\n</messages>",
            self.summary_instructions
        )
    }
}

/// A [`CompactionStrategy`] reproducing `SummarizationMiddleware.before_model`.
///
/// The order of operations is taken from the current SDK, not invented here:
///
/// 1. System-authored events are lifted out of the working set and, when
///    `preserve_system_messages` is set, re-emitted verbatim at the head. They
///    are never summarized and never dropped.
/// 2. The last `preserve_recent_messages` events form an untouchable tail.
/// 3. Compaction is skipped entirely while the transcript ends inside a
///    tool-call loop — summarizing there would break call/response pairing.
/// 4. The remaining budget is `max_context_tokens` minus everything already
///    pinned; older events are walked back from the tail and kept while they
///    fit, so only the events beyond the budget are summarized.
/// 5. The summarized head is replaced by a single summary event carrying the
///    SDK's `Here is a summary of the conversation to date:` prefix.
pub(super) struct EliteaContextCompaction {
    summarizer: Arc<dyn BaseEventsSummarizer>,
    preserve_recent_messages: usize,
    preserve_system_messages: bool,
}

impl EliteaContextCompaction {
    /// Split the transcript into pinned system events and the working set.
    fn partition_system(&self, events: Vec<Event>) -> (Vec<Event>, Vec<Event>) {
        if !self.preserve_system_messages {
            return (Vec::new(), events);
        }
        events.into_iter().partition(is_system_event)
    }

    /// Number of head events that must be summarized to reach the budget.
    fn cutoff_index(head: &[Event], budget: usize, pinned_tokens: usize) -> usize {
        let remaining = budget.saturating_sub(pinned_tokens);
        let mut spent = 0usize;
        let mut fitting = 0usize;
        for event in head.iter().rev() {
            let cost = estimate_event_tokens(std::slice::from_ref(event));
            if spent.saturating_add(cost) > remaining {
                break;
            }
            spent = spent.saturating_add(cost);
            fitting = fitting.saturating_add(1);
        }
        head.len() - fitting
    }
}

#[async_trait]
impl CompactionStrategy for EliteaContextCompaction {
    async fn compact(&self, events: Vec<Event>, budget: usize) -> Result<Vec<Event>, AdkError> {
        // Never compact mid tool-call loop: the pending call and its response
        // must reach the model together.
        if events.last().is_some_and(is_tool_related_event) {
            return Ok(events);
        }
        let (system, working) = self.partition_system(events);
        if working.len() <= self.preserve_recent_messages {
            return Ok(rejoin(system, Vec::new(), working));
        }
        let tail_start = working.len() - self.preserve_recent_messages;
        let mut head = working;
        let tail = head.split_off(tail_start);
        let pinned_tokens =
            estimate_event_tokens(&system).saturating_add(estimate_event_tokens(&tail));
        let cutoff = Self::cutoff_index(&head, budget, pinned_tokens);
        if cutoff == 0 {
            return Ok(rejoin(system, head, tail));
        }
        let mut kept_head = head;
        let summarize = kept_head.drain(..cutoff).collect::<Vec<_>>();
        let Some(summary) = self.summarizer.summarize_events(&summarize).await? else {
            return Ok(rejoin(system, [summarize, kept_head].concat(), tail));
        };
        let Some(text) = compacted_text(&summary) else {
            return Ok(rejoin(system, [summarize, kept_head].concat(), tail));
        };
        tracing::info!(
            summarized = summarize.len(),
            kept = kept_head.len(),
            preserved_tail = tail.len(),
            pinned_system = system.len(),
            budget,
            "compacted the native agent transcript for this invocation"
        );
        let mut rolled_up = summary_event(&text);
        // Keep the summary ordered ahead of everything it replaced.
        if let Some(first) = summarize.first() {
            rolled_up.timestamp = first.timestamp;
        }
        let mut replaced = Vec::with_capacity(1 + kept_head.len());
        replaced.push(rolled_up);
        replaced.append(&mut kept_head);
        Ok(rejoin(system, replaced, tail))
    }
}

/// Reassemble pinned system events, the middle, and the untouchable tail.
fn rejoin(system: Vec<Event>, middle: Vec<Event>, tail: Vec<Event>) -> Vec<Event> {
    let mut rejoined = Vec::with_capacity(system.len() + middle.len() + tail.len());
    rejoined.extend(system);
    rejoined.extend(middle);
    rejoined.extend(tail);
    rejoined
}

/// Build the SDK-shaped summary turn.
///
/// The event carries plain content rather than an `EventCompaction` marker:
/// a marker would make ADK's history builder drop every event older than the
/// compaction boundary, including the system events this strategy just pinned.
fn summary_event(text: &str) -> Event {
    let mut event = Event::new("elitea-context-compaction");
    "user".clone_into(&mut event.author);
    event.set_content(
        Content::new("user").with_text(format!("{SUMMARY_PREFIX}\n\n{}", text.trim())),
    );
    event
}

/// Read the summary text out of whichever shape the summarizer produced.
fn compacted_text(summary: &Event) -> Option<String> {
    let content = summary.actions.compaction.as_ref().map_or_else(
        || summary.content(),
        |compaction| Some(&compaction.compacted_content),
    )?;
    let text = content
        .parts
        .iter()
        .filter_map(|part| match part {
            Part::Text { text } => Some(text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n");
    (!text.trim().is_empty()).then_some(text)
}

/// Does this event carry the system prompt rather than a conversation turn?
fn is_system_event(event: &Event) -> bool {
    event.author == "system"
        || event
            .content()
            .is_some_and(|content| content.role == "system")
}

/// Is this event part of a tool-call interaction rather than a user-facing turn?
fn is_tool_related_event(event: &Event) -> bool {
    event.content().is_some_and(|content| {
        content.parts.iter().any(|part| {
            matches!(
                part,
                Part::FunctionCall { .. } | Part::FunctionResponse { .. }
            )
        })
    })
}

/// Read a boolean setting, defaulting when absent or null.
fn admit_bool(
    settings: &Map<String, Value>,
    key: &str,
    default: bool,
) -> Result<bool, NativeAgentAssemblyError> {
    match settings.get(key) {
        None | Some(Value::Null) => Ok(default),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(invalid_input()),
    }
}

/// Read an integer setting, defaulting when absent or null and refusing
/// anything outside the frozen range.
fn admit_u64(
    settings: &Map<String, Value>,
    key: &str,
    default: u64,
    min: u64,
    max: u64,
) -> Result<u64, NativeAgentAssemblyError> {
    let value = match settings.get(key) {
        None | Some(Value::Null) => return Ok(default),
        Some(Value::Number(number)) => number.as_u64().ok_or_else(invalid_input)?,
        Some(_) => return Err(invalid_input()),
    };
    if !(min..=max).contains(&value) {
        return Err(invalid_input());
    }
    Ok(value)
}

const fn unsupported() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "native agent context management is not enabled",
    )
}

const fn invalid_input() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidInput,
        "native agent context settings are malformed",
    )
}
