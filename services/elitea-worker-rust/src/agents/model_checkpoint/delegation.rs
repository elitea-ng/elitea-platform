//! Replay saved-agent delegation through normal ADK dispatch, never arbitrary effects.

use super::{CHECKPOINT_KEY, Checkpoint, ModelCheckpointWriter, Phase, invalid_checkpoint};
use adk_rust::session::{AppendEventRequest, GetRequest};
use adk_rust::{
    AdkIdentity, CallbackContext, Content, Event, Llm, LlmRequest, LlmResponse, LlmResponseStream,
    Part,
};
use async_trait::async_trait;
use serde_json::json;
use std::collections::HashSet;
use std::sync::{Arc, Mutex};

/// A stored durable batch's original model event id (C2).
pub(super) fn valid_batch_event_id(batch: &str) -> bool {
    !batch.is_empty() && batch.len() <= 512 && !batch.chars().any(char::is_control)
}

pub(super) fn validate_calls(
    content: &Content,
    admitted: impl Fn(&str) -> bool,
) -> adk_rust::Result<()> {
    let mut identities = HashSet::new();
    let mut count = 0;
    if content.role != "model" {
        return Err(invalid_checkpoint());
    }
    for part in &content.parts {
        match part {
            Part::FunctionCall {
                name,
                args,
                id: Some(id),
                ..
            } if admitted(name)
                && args.is_object()
                && !id.is_empty()
                && id.len() <= 256
                && identities.insert(id) =>
            {
                count += 1;
            }
            Part::Text { .. } | Part::Thinking { .. } => {}
            _ => return Err(invalid_checkpoint()),
        }
    }
    if count == 0 || count > 8 {
        return Err(invalid_checkpoint());
    }
    Ok(())
}

impl ModelCheckpointWriter {
    pub(in crate::agents) fn with_application_tools<'a>(
        mut self,
        names: impl Iterator<Item = &'a str>,
    ) -> Self {
        self.application_tools = Arc::new(names.map(str::to_owned).collect());
        self
    }

    /// The pipeline tools this Agent runs on a durable thread (FU-291).
    pub(in crate::agents) fn with_durable_pipeline_tools<'a>(
        mut self,
        names: impl Iterator<Item = &'a str>,
    ) -> Self {
        self.durable_pipeline_tools = Arc::new(names.map(str::to_owned).collect());
        self
    }

    /// C2/C4: a durable record replays only where this Worker binds its call
    /// to its durable thread, and always under its original batch event; any
    /// other record carries no batch id.
    pub(super) fn durable_replay_batch(
        &self,
        phase: Phase,
        content: &Content,
        batch: Option<String>,
    ) -> adk_rust::Result<Option<String>> {
        if !matches!(phase, Phase::DurableDelegationPending) {
            return if batch.is_some() {
                Err(invalid_checkpoint())
            } else {
                Ok(None)
            };
        }
        if !self.holds_durable_call(content) {
            return Err(invalid_checkpoint());
        }
        batch
            .filter(|batch| valid_batch_event_id(batch))
            .map(Some)
            .ok_or_else(invalid_checkpoint)
    }

    /// Whether `content` calls at least one of this writer's durable pipeline tools.
    pub(super) fn holds_durable_call(&self, content: &Content) -> bool {
        content.parts.iter().any(|part| {
            matches!(part, Part::FunctionCall { name, .. }
                if self.durable_pipeline_tools.contains(name))
        })
    }

    pub(in crate::agents) fn delegation_model(&self, inner: Arc<dyn Llm>) -> Arc<dyn Llm> {
        match &self.replay_delegation {
            Some(pending) => Arc::new(DelegationReplayModel {
                inner,
                pending: pending.clone(),
                batch: self.replay_batch.clone(),
            }),
            None => inner,
        }
    }

    pub(super) fn pending_delegation(&self) -> adk_rust::Result<Option<Content>> {
        self.replay_delegation.as_ref().map_or(Ok(None), |pending| {
            pending
                .lock()
                .map(|content| content.clone())
                .map_err(|_| invalid_checkpoint())
        })
    }

    pub(super) async fn before_application_or_tool(
        &self,
        ctx: &dyn CallbackContext,
    ) -> adk_rust::Result<()> {
        let identity = ctx.try_identity()?;
        self.before_application_or_tool_at(ctx, identity).await
    }

    pub(in crate::agents) async fn before_application_or_tool_at(
        &self,
        ctx: &dyn CallbackContext,
        identity: AdkIdentity,
    ) -> adk_rust::Result<()> {
        if let Some(name) = ctx.tool_name()
            && self.replayable(name)
            && self
                .persist_pending_application(ctx, identity.clone())
                .await?
        {
            return Ok(());
        }
        self.before_tool(identity, ctx.invocation_id()).await
    }

    async fn persist_pending_application(
        &self,
        ctx: &dyn CallbackContext,
        identity: AdkIdentity,
    ) -> adk_rust::Result<bool> {
        let session = self
            .sessions
            .get(GetRequest {
                app_name: identity.app_name.to_string(),
                user_id: identity.user_id.to_string(),
                session_id: identity.session_id.to_string(),
                num_recent_events: None,
                after: None,
            })
            .await?;
        let value = session
            .state()
            .get(CHECKPOINT_KEY)
            .ok_or_else(invalid_checkpoint)?;
        let checkpoint: Checkpoint =
            serde_json::from_value(value.clone()).map_err(|_| invalid_checkpoint())?;
        if checkpoint.version != 1
            || checkpoint.execution_id != self.execution_id
            || checkpoint.generation != self.generation
            || checkpoint.definition_digest != self.definition_digest
            || checkpoint.invocation_id != ctx.invocation_id()
        {
            return Err(invalid_checkpoint());
        }
        // A prior ordinary effect cannot be upgraded into replay permission.
        if !matches!(
            checkpoint.phase,
            Phase::ModelPending | Phase::DelegationPending | Phase::DurableDelegationPending
        ) {
            return Ok(false);
        }
        let events = session.events().all();
        let marker = events
            .iter()
            .rposition(|event| {
                event.author == "elitea-recovery"
                    && event.invocation_id == checkpoint.invocation_id
                    && event.actions.state_delta.get(CHECKPOINT_KEY) == Some(&value)
            })
            .ok_or_else(invalid_checkpoint)?;
        let (content, batch) = match checkpoint.phase {
            Phase::DelegationPending | Phase::DurableDelegationPending => (
                checkpoint.delegation.ok_or_else(invalid_checkpoint)?,
                checkpoint.batch_event_id,
            ),
            Phase::ModelPending => {
                // C2: the batch's own model event names it for good; a replay
                // after a crash re-emits it under this id.
                let Some((content, batch)) = events[marker + 1..]
                    .iter()
                    .rev()
                    .find(|event| {
                        event.author == ctx.agent_name()
                            && event.invocation_id == ctx.invocation_id()
                            && !event.llm_response.partial
                            && event.llm_response.content.is_some()
                    })
                    .and_then(|event| {
                        event
                            .llm_response
                            .content
                            .clone()
                            .map(|content| (content, Some(original_batch_id(event))))
                    })
                else {
                    return Err(invalid_checkpoint());
                };
                (content, batch)
            }
            _ => return Err(invalid_checkpoint()),
        };
        if validate_calls(&content, |name| self.replayable(name)).is_err() {
            return Ok(false);
        }
        if !content.parts.iter().any(|part| {
            matches!(part,
                Part::FunctionCall {name, args, ..}
                    if Some(name.as_str()) == ctx.tool_name() && Some(args) == ctx.tool_input()
            )
        }) {
            return Err(invalid_checkpoint());
        }
        let mut model = checkpoint.model.ok_or_else(invalid_checkpoint)?;
        model.request.tools = model.tools;
        self.persist_delegation(
            identity,
            ctx.invocation_id(),
            &model.request,
            &content,
            batch,
        )
        .await?;
        Ok(true)
    }

    /// `batch` is the batch's original model event (C2); a durable pipeline
    /// batch is never recorded without it.
    pub(super) async fn persist_delegation(
        &self,
        identity: AdkIdentity,
        invocation: &str,
        request: &LlmRequest,
        content: &Content,
        batch: Option<String>,
    ) -> adk_rust::Result<()> {
        let mut record = json!({
            "version": 1, "execution_id": self.execution_id, "generation": self.generation,
            "definition_digest": self.definition_digest, "invocation_id": invocation,
            "phase": Phase::DelegationPending,
            "model": {"request": request, "tools": request.tools}, "delegation": content,
        });
        if self.holds_durable_call(content) {
            let batch = batch
                .filter(|batch| valid_batch_event_id(batch))
                .ok_or_else(invalid_checkpoint)?;
            record["phase"] = json!(Phase::DurableDelegationPending);
            record["batch_event_id"] = json!(batch);
        }
        let mut event = Event::new(invocation);
        "elitea-recovery".clone_into(&mut event.author);
        event
            .actions
            .state_delta
            .insert(CHECKPOINT_KEY.into(), record);
        self.sessions
            .append_event_for_identity(AppendEventRequest { identity, event })
            .await
    }
}

struct DelegationReplayModel {
    inner: Arc<dyn Llm>,
    pending: Arc<Mutex<Option<Content>>>,
    /// A durable pipeline batch is re-emitted under its original event (C2).
    batch: Option<String>,
}

/// The id a batch event names its calls by: the original of a replayed batch.
fn original_batch_id(event: &Event) -> String {
    crate::agents::application_tools::original_application_batch_id(event)
        .unwrap_or_else(|_| event.id.clone())
}

#[async_trait]
impl Llm for DelegationReplayModel {
    fn name(&self) -> &str {
        self.inner.name()
    }
    fn schema_adapter(&self) -> &dyn adk_rust::schema_adapter::SchemaAdapter {
        self.inner.schema_adapter()
    }
    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }
    async fn generate_content(
        &self,
        request: LlmRequest,
        stream: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        let content = self
            .pending
            .lock()
            .map_err(|_| invalid_checkpoint())?
            .take();
        if let Some(content) = content {
            let response = LlmResponse {
                content: Some(content),
                turn_complete: true,
                finish_reason: Some(adk_rust::FinishReason::Stop),
                provider_metadata: self.batch.as_ref().map(|batch| {
                    json!({crate::agents::application_tools::DURABLE_REPLAY_BATCH_KEY: batch})
                }),
                ..LlmResponse::default()
            };
            return Ok(Box::pin(adk_rust::futures::stream::once(async {
                Ok(response)
            })));
        }
        self.inner.generate_content(request, stream).await
    }
}

#[cfg(test)]
#[path = "delegation_tests.rs"]
mod tests;
