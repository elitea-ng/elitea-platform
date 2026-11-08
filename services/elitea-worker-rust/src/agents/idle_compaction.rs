//! Idle context compaction of one (conversation, agent) in the cloud
//! (ADR-0050 decision 9b2, issue 1610).
//!
//! Main admits an idle compaction job from the SOURCE execution's frozen
//! input: the same thread, definition and model, an empty `user_input`, no
//! continuation fields, and `payload.meta.idle_compaction_v1`. The worker
//! then never runs the agent loop. It replays the source turn's last model
//! request (the `model_pending` checkpoint the turn persisted) byte for byte,
//! appends the model's final answer and one continuation prompt, and asks
//! once for a v1 continuation record (desktop `d0/idle.rs` is the local
//! twin). The identical prefix reads the provider's prompt cache. The
//! validated record becomes the durable root compaction record
//! (`context_compaction::STATE_KEY`) that the next turn's
//! `DurableContextCompaction::prepare` splices in.
//!
//! Nothing here appends a user or model message, calls a tool or emits a
//! transcript frame: the only frame is one `agent_context_status` (phase
//! `compacted`) measuring the request the next turn would send.

use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::session::{AppendEventRequest, GetRequest, Session, SessionService};
use adk_rust::{
    AdkError, AdkIdentity, Agent, Content, ErrorCategory, ErrorComponent, Event, EventStream,
    InvocationContext, Llm, LlmRequest, Part,
};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::{Value, json};

use super::context_budget::ModelRequestBudget;
use super::context_compaction::{CompactionRecord, STATE_KEY, checkpoint_content, extend_digest};
use super::model_checkpoint::{CHECKPOINT_KEY, HISTORY_SNAPSHOT_KEY};
use super::request::{AgentExecutionPayload, UserInput};

/// `payload.meta` member carrying the idle compaction marker.
pub(crate) const IDLE_COMPACTION_META_KEY: &str = "idle_compaction_v1";
const IDLE_COMPACTION_SCHEMA: &str = "elitea.agent.idle-compaction.v1";
/// Below this many estimated input tokens the cached summary call costs more
/// than it saves (desktop `d0/idle.rs` `MIN_MESSAGE_TOKENS`).
pub(crate) const MIN_REQUEST_TOKENS: u64 = 60_000;
/// The author of the event that commits an idle compaction record.
const IDLE_AUTHOR: &str = "elitea-idle-compaction";

/// A validated idle compaction marker.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct IdleCompaction {
    source_execution_id: String,
    source_generation: u64,
}

impl IdleCompaction {
    /// `Ok(None)` when the payload carries no marker. A marker is admitted
    /// only on an otherwise empty input; anything else is a typed refusal,
    /// never a fresh turn.
    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Option<Self>, IdleCompactionError> {
        let Some(marker) = payload.meta.get(IDLE_COMPACTION_META_KEY) else {
            return Ok(None);
        };
        let marker = Self::parse(marker)?;
        let empty_input = matches!(&payload.user_input, UserInput::Text(text) if text.is_empty());
        if payload.meta.len() != 1
            || !empty_input
            || payload.hitl_resume
            || payload.should_continue
            || payload.hitl_action.is_some()
            || payload.hitl_value.is_some()
            || !payload.hitl_decisions.is_empty()
            || payload.is_regenerate
            || payload.truncated_content.is_some()
            || payload.checkpoint_id.is_some()
            || payload.parallel_reconcile.is_some()
            || !payload.parallel_terminal_errors.is_empty()
            || !payload.ignored_mcp_servers.is_empty()
            || !payload.user_declined_mcp_servers.is_empty()
        {
            return Err(IdleCompactionError::NotIdle);
        }
        Ok(Some(marker))
    }

    /// Exactly five members: the schema constant, a cache TTL and a request
    /// start (both positive), the source execution (32 lowercase hex) and
    /// its generation (`>= 1`).
    fn parse(value: &Value) -> Result<Self, IdleCompactionError> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Marker {
            schema: String,
            cache_ttl_seconds: i64,
            request_started_unix_ms: i64,
            source_execution_id: String,
            source_generation: i64,
        }
        let marker =
            Marker::deserialize(value).map_err(|_| IdleCompactionError::MalformedMarker)?;
        let source_generation = u64::try_from(marker.source_generation)
            .ok()
            .filter(|value| *value >= 1);
        match source_generation {
            Some(source_generation)
                if marker.schema == IDLE_COMPACTION_SCHEMA
                    && marker.cache_ttl_seconds > 0
                    && marker.request_started_unix_ms > 0
                    && valid_execution_id(&marker.source_execution_id) =>
            {
                Ok(Self {
                    source_execution_id: marker.source_execution_id,
                    source_generation,
                })
            }
            _ => Err(IdleCompactionError::MalformedMarker),
        }
    }

    #[cfg(test)]
    pub(crate) fn source(&self) -> (&str, u64) {
        (&self.source_execution_id, self.source_generation)
    }
}

/// Whether the payload asks for an idle compaction (its marker is present).
pub(crate) fn requested(payload: &AgentExecutionPayload) -> bool {
    payload.meta.contains_key(IDLE_COMPACTION_META_KEY)
}

fn valid_execution_id(value: &str) -> bool {
    value.len() == 32
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub(crate) enum IdleCompactionError {
    /// The marker does not match the v1 schema.
    #[error("agent.idle_compaction.malformed")]
    MalformedMarker,
    /// The marker rides on an input with text, a decision or another
    /// continuation kind.
    #[error("agent.idle_compaction.not_empty")]
    NotIdle,
    /// The marker rides on a pipeline (graph) agent.
    #[error("agent.idle_compaction.pipeline")]
    Pipeline,
}

impl IdleCompactionError {
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::MalformedMarker => "agent.idle_compaction.malformed",
            Self::NotIdle => "agent.idle_compaction.not_empty",
            Self::Pipeline => "agent.idle_compaction.pipeline",
        }
    }
}

/// The source turn's last request and the answer that closed it.
struct LastTurn {
    request: LlmRequest,
    answer: Content,
}

/// The latest root `model_pending` request of `agent_name` and the model's
/// final answer after it. `None` when the session has no such pair (the
/// last request was not answered, or was answered with a tool call).
fn last_turn(
    session: &dyn Session,
    definition_digest: [u8; 32],
    agent_name: &str,
) -> adk_rust::Result<Option<LastTurn>> {
    let events = session.events().all();
    let Some(index) = events.iter().rposition(|event| {
        event.author == "elitea-recovery"
            && event.branch.is_empty()
            && event.actions.state_delta.contains_key(HISTORY_SNAPSHOT_KEY)
            && event
                .actions
                .state_delta
                .get(CHECKPOINT_KEY)
                .is_some_and(|checkpoint| checkpoint["phase"] == "model_pending")
    }) else {
        return Ok(None);
    };
    let marker = &events[index];
    let checkpoint = &marker.actions.state_delta[CHECKPOINT_KEY];
    if checkpoint["definition_digest"] != json!(definition_digest) {
        return Ok(None);
    }
    let Some(model) = checkpoint.get("model").filter(|model| !model.is_null()) else {
        return Ok(None);
    };
    let mut request: LlmRequest =
        serde_json::from_value(model["request"].clone()).map_err(|_| invalid_idle())?;
    // ADK omits `tools` from `LlmRequest` serialization; the checkpoint keeps
    // them beside it so the replayed prefix declares the same tools.
    request.tools = serde_json::from_value(model["tools"].clone()).map_err(|_| invalid_idle())?;
    let answer = events[index + 1..].iter().rev().find(|event| {
        event.invocation_id == marker.invocation_id
            && event.author == agent_name
            && event.branch.is_empty()
            && !event.llm_response.partial
            && event.llm_response.content.is_some()
    });
    let Some(answer) = answer.and_then(|event| event.llm_response.content.clone()) else {
        return Ok(None);
    };
    // A later checkpoint, tool call or user message means the turn did not
    // end on this request's answer.
    if events[index + 1..].iter().any(|event| {
        event.author == "user"
            || (event.author == "elitea-recovery"
                && event.actions.state_delta.contains_key(CHECKPOINT_KEY))
    }) || answer.parts.iter().any(|part| {
        matches!(
            part,
            Part::FunctionCall { .. } | Part::FunctionResponse { .. }
        )
    }) || !answer
        .parts
        .iter()
        .any(|part| matches!(part, Part::Text { text } if !text.trim().is_empty()))
    {
        return Ok(None);
    }
    let mut answer = answer;
    "model".clone_into(&mut answer.role);
    Ok(Some(LastTurn { request, answer }))
}

/// What one idle compaction did.
#[derive(Debug, Eq, PartialEq)]
pub(crate) enum IdleOutcome {
    /// Nothing to do: Main records the job as skipped.
    Skipped(&'static str),
    /// The record was committed; the next request is this many tokens.
    Compacted { estimated_input: u64 },
}

/// The dependencies of one idle compaction run.
pub(crate) struct IdleCompactionRun {
    pub(crate) sessions: Arc<dyn SessionService>,
    pub(crate) identity: AdkIdentity,
    pub(crate) model: Arc<dyn Llm>,
    pub(crate) budget: Arc<dyn ModelRequestBudget>,
    pub(crate) definition_digest: [u8; 32],
    pub(crate) agent_name: String,
    pub(crate) guidance: String,
    /// The bound facade's completion: each summary call starts from an
    /// empty one, since the facade records one completion per call.
    pub(crate) completion: Option<Arc<dyn super::session::DurableModelCompletion>>,
}

impl IdleCompactionRun {
    /// Run once; `status` receives the one `compacted` status event.
    #[allow(clippy::too_many_lines)] // Keep the skip checks, the call and the commit in order.
    pub(crate) async fn run(
        &self,
        invocation_id: &str,
        status: impl FnOnce(Event),
    ) -> adk_rust::Result<IdleOutcome> {
        let session = self
            .sessions
            .get(GetRequest {
                app_name: self.identity.app_name.to_string(),
                user_id: self.identity.user_id.to_string(),
                session_id: self.identity.session_id.to_string(),
                num_recent_events: None,
                after: None,
            })
            .await;
        let session = match session {
            Ok(session) => session,
            Err(error) if error.code == "session.not_found" => {
                return Ok(IdleOutcome::Skipped("no_session"));
            }
            Err(error) => return Err(error),
        };
        let Some(last) = last_turn(session.as_ref(), self.definition_digest, &self.agent_name)?
        else {
            return Ok(IdleOutcome::Skipped("no_answered_request"));
        };
        // The working history the next turn's `prepare` sees: every content
        // after the pinned system contents and the first user content.
        let mut first_user = None;
        let mut working = Vec::new();
        for content in &last.request.contents {
            if content.role == "system" {
                continue;
            }
            if content.role == "user" && first_user.is_none() {
                first_user = Some(content.clone());
            } else {
                working.push(content.clone());
            }
        }
        let Some(first_user) = first_user else {
            return Ok(IdleOutcome::Skipped("no_anchor"));
        };
        let anchor = extend_digest([0; 32], std::slice::from_ref(&first_user))?;
        // Coverage is counted over the history as the next turn's view sees
        // it: the snapshot of this request (an earlier summary included, as
        // it was sent) and then the answer. Both the Runner's projection
        // (`runner_history::project_compaction`) and `prepare` accept it.
        let mut covered = working;
        covered.push(last.answer.clone());
        let covered_count = covered.len();
        let covered_digest = extend_digest([0; 32], &covered)?;
        let saved = super::context_compaction::saved_record(session.as_ref())?;
        if saved.as_ref().is_some_and(|saved| {
            saved.covers(
                self.definition_digest,
                anchor,
                covered_count,
                covered_digest,
            )
        }) {
            return Ok(IdleOutcome::Skipped("already_compacted"));
        }
        let before = self
            .budget
            .measure(&super::replay_history::model_history(last.request.clone())?)?;
        if before.estimated_input < MIN_REQUEST_TOKENS {
            return Ok(IdleOutcome::Skipped("below_threshold"));
        }
        // References must occur in what the model saw: the request as sent.
        let source = json!({"messages": last.request.contents, "answer": last.answer});
        let summary = self.summarize(&last, &source).await?;
        let replacement = vec![checkpoint_content(&summary)];
        let record = CompactionRecord::covering(
            self.definition_digest,
            anchor,
            covered_count,
            covered_digest,
            replacement.clone(),
        );
        // The request the next turn would send, before its own user input.
        let mut next = last.request.clone();
        next.contents = last
            .request
            .contents
            .iter()
            .filter(|content| content.role == "system")
            .cloned()
            .chain(std::iter::once(first_user))
            .chain(replacement)
            .collect();
        let after = self
            .budget
            .measure(&super::replay_history::model_history(next)?)?;
        let mut event = Event::new(invocation_id);
        IDLE_AUTHOR.clone_into(&mut event.author);
        event.actions.state_delta.insert(
            STATE_KEY.to_owned(),
            serde_json::to_value(&record).map_err(|_| invalid_idle())?,
        );
        // The claim-fenced session writer refuses this append once another
        // claim has taken the session over.
        self.sessions
            .append_event_for_identity(AppendEventRequest {
                identity: self.identity.clone(),
                event,
            })
            .await?;
        status(
            super::context_status::ModelContextStatus::new(
                super::context_status::ContextPhase::Compacted,
                after,
            )
            .event()?,
        );
        Ok(IdleOutcome::Compacted {
            estimated_input: after.estimated_input,
        })
    }

    /// The validated v1 record: the last request, the answer and the
    /// continuation prompt; once more with the template retry on a miss.
    async fn summarize(&self, last: &LastTurn, source: &Value) -> adk_rust::Result<String> {
        let mut request = last.request.clone();
        request.contents.push(last.answer.clone());
        request.contents.push(
            Content::new("user")
                .with_text(super::context_summary::continuation_prompt(&self.guidance)),
        );
        let first = self.ask(request.clone()).await?;
        match super::context_summary::validate(&first, source) {
            Ok(accepted) => Ok(accepted),
            Err(error) => {
                tracing::info!(
                    validation_code = error.code,
                    "the idle summary ignored the template: asking once more"
                );
                request
                    .contents
                    .push(Content::new("model").with_text(first));
                request.contents.push(
                    Content::new("user").with_text(super::context_management::TEMPLATE_RETRY),
                );
                let second = self.ask(request).await?;
                super::context_summary::validate(&second, source)
            }
        }
    }

    /// One provider call through the bound facade; an answer that calls a
    /// tool is refused, never executed.
    async fn ask(&self, request: LlmRequest) -> adk_rust::Result<String> {
        if let Some(completion) = &self.completion {
            completion.discard_unaccepted()?;
        }
        let mut responses = self.model.generate_content(request, true).await?;
        let mut text = String::new();
        let mut terminal = false;
        while let Some(response) = responses.next().await {
            let response = response?;
            if let Some(content) = &response.content {
                for part in &content.parts {
                    match part {
                        Part::FunctionCall { .. } => return Err(tool_call_refused()),
                        // Deltas, then the terminal's own last delta (never a
                        // restatement at the facade boundary).
                        Part::Text { text: delta } => text.push_str(delta),
                        _ => {}
                    }
                }
            }
            if !response.partial {
                terminal = true;
            }
        }
        if !terminal || text.trim().is_empty() {
            return Err(invalid_idle());
        }
        Ok(text)
    }
}

/// The root agent of an idle compaction invocation: it runs the compaction
/// and yields only the status event. It never calls the agent loop.
pub(crate) struct IdleCompactionAgent {
    name: String,
    /// `None` when the run does not use durable root compaction: nothing to
    /// write, so the job completes doing nothing.
    run: Option<Arc<IdleCompactionRun>>,
}

impl IdleCompactionAgent {
    pub(crate) fn new(name: &str, run: Option<IdleCompactionRun>) -> Self {
        Self {
            name: name.to_owned(),
            run: run.map(Arc::new),
        }
    }
}

#[async_trait]
impl Agent for IdleCompactionAgent {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &'static str {
        "idle context compaction"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, ctx: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let Some(run) = self.run.clone() else {
            tracing::info!(
                event = "agent_idle_compaction_finished",
                outcome = "no_durable_compaction",
                "the idle compaction finished"
            );
            return Ok(Box::pin(adk_rust::futures::stream::empty()));
        };
        let invocation_id = ctx.invocation_id().to_owned();
        let author = self.name.clone();
        Ok(Box::pin(async_stream::stream! {
            let mut status = None;
            match run.run(&invocation_id, |event| status = Some(event)).await {
                Ok(outcome) => tracing::info!(
                    event = "agent_idle_compaction_finished",
                    outcome = ?outcome,
                    "the idle compaction finished"
                ),
                Err(error) => {
                    yield Err(error);
                    return;
                }
            }
            if let Some(mut event) = status {
                event.invocation_id.clone_from(&invocation_id);
                author.clone_into(&mut event.author);
                yield Ok(event);
            }
        }))
    }
}

fn tool_call_refused() -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::InvalidInput,
        "agent.idle_compaction.tool_call",
        "The idle summary answer called a tool.",
    )
}

fn invalid_idle() -> AdkError {
    AdkError::new(
        ErrorComponent::Session,
        ErrorCategory::InvalidInput,
        "agent.idle_compaction.invalid",
        "The idle compaction cannot use this session's last request.",
    )
}

#[cfg(test)]
#[path = "idle_compaction_tests.rs"]
mod tests;
