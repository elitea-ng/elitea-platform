//! Persist scoped summary coverage with the pending model request.

use std::collections::HashSet;
use std::sync::{Arc, Mutex};

use adk_rust::agent::LlmEventSummarizer;
use adk_rust::session::Session;
use adk_rust::{
    AdkError, BaseEventsSummarizer, Content, ErrorCategory, ErrorComponent, Event, Llm, LlmRequest,
    Part,
};
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::context_budget::ModelRequestBudget;
use super::context_management::ContextCompactionPlan;

pub(super) const STATE_KEY: &str = "elitea.context.root.v1";

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct CompactionRecord {
    version: u32,
    definition_digest: [u8; 32],
    anchor: [u8; 32],
    covered_count: usize,
    covered_digest: [u8; 32],
    replacement: Vec<Content>,
}

/// This instance belongs to one ordinary root Runner. Child scopes bind separately.
pub(super) struct DurableContextCompaction {
    plan: ContextCompactionPlan,
    budget: Arc<dyn ModelRequestBudget>,
    summarizer: Arc<dyn BaseEventsSummarizer>,
    definition_digest: [u8; 32],
    record: Mutex<Option<CompactionRecord>>,
}

impl DurableContextCompaction {
    pub(super) fn new(
        plan: ContextCompactionPlan,
        budget: Arc<dyn ModelRequestBudget>,
        model: Arc<dyn Llm>,
        definition_digest: [u8; 32],
        session: &dyn Session,
    ) -> adk_rust::Result<Self> {
        let record = session
            .state()
            .get(STATE_KEY)
            .filter(|value| !value.is_null())
            .map(serde_json::from_value::<CompactionRecord>)
            .transpose()
            .map_err(|_| invalid_compaction())?;
        if record.as_ref().is_some_and(|record| {
            record.version != 1
                || record.covered_count == 0
                || record.replacement.is_empty()
                || record.replacement.len() > 2
                || record.replacement.iter().any(|content| {
                    content.role != "user"
                        || content.parts.is_empty()
                        || content.parts.iter().any(
                            |part| !matches!(part, Part::Text { text } if !text.trim().is_empty()),
                        )
                })
        }) {
            return Err(invalid_compaction());
        }
        let record = record.filter(|record| record.definition_digest == definition_digest);
        let summarizer = Arc::new(
            LlmEventSummarizer::new(model)
                .with_prompt_template(super::context_summary::prompt(&plan.prompt_template())),
        );
        Ok(Self {
            plan,
            budget,
            summarizer,
            definition_digest,
            record: Mutex::new(record),
        })
    }

    /// Prepare without changing memory or storage. The checkpoint writer commits both.
    #[allow(clippy::too_many_lines)] // Keep coverage, preparation, and admission checks in execution order.
    pub(super) async fn prepare<F, Fut>(
        &self,
        mut request: LlmRequest,
        before_summary: F,
    ) -> adk_rust::Result<(LlmRequest, Option<CompactionRecord>)>
    where
        F: FnOnce() -> Fut + Send,
        Fut: std::future::Future<Output = adk_rust::Result<()>> + Send,
    {
        let mut pinned = Vec::new();
        let mut working = Vec::new();
        let mut first_user = None;
        for content in std::mem::take(&mut request.contents) {
            if content.role == "system" {
                pinned.push(content);
            } else if content.role == "user" && first_user.is_none() {
                first_user = Some(content);
            } else {
                working.push(content);
            }
        }
        let first_user = first_user.ok_or_else(invalid_compaction)?;
        let anchor = extend_digest([0; 32], std::slice::from_ref(&first_user))?;
        pinned.push(first_user);
        let mut record = self
            .record
            .lock()
            .map_err(|_| invalid_compaction())?
            .clone();
        if let Some(saved) = record.as_ref() {
            if saved.anchor != anchor {
                record = None;
            } else if working.len() >= saved.covered_count
                && extend_digest([0; 32], &working[..saved.covered_count])? == saved.covered_digest
            {
                working.splice(..saved.covered_count, saved.replacement.clone());
            } else if !starts_with(&working, &saved.replacement) {
                // Edited or regenerated source history invalidates the summary.
                // Keep every original record and calculate fresh coverage.
                record = None;
            }
        }
        request.contents = joined(&pinned, &working);
        let before = self.budget.measure(&request)?;
        if !before.needs_compaction() {
            return Ok((request, record));
        }
        let previous_len = record.as_ref().map_or(0, |value| value.replacement.len());
        let cutoff = complete_prefix(&working, self.plan.preserve_recent_messages)?;
        if cutoff <= previous_len {
            before.check()?;
            return Ok((request, record));
        }
        let protected_user = working
            .iter()
            .enumerate()
            .rposition(|(index, content)| {
                content.role == "user" && !(record.is_some() && index == 0)
            })
            .filter(|index| *index < cutoff)
            .map(|index| working[index].clone());
        let minimum = protected_user
            .iter()
            .chain(&working[cutoff..])
            .cloned()
            .collect::<Vec<_>>();
        request.contents = joined(&pinned, &minimum);
        self.budget.measure(&request)?.check()?;
        before_summary().await?;
        let text = self
            .summarize(
                pinned.last().ok_or_else(invalid_compaction)?,
                &working[..cutoff],
            )
            .await?;
        let mut replacement = vec![Content::new("user").with_text(format!(
            "Summary of earlier conversation records. Authoritative instructions remain separate.\n{text}"
        ))];
        if let Some(user) = protected_user {
            replacement.push(user);
        }
        let (covered_count, covered_digest) = if let Some(saved) = record {
            (
                saved
                    .covered_count
                    .checked_add(cutoff - previous_len)
                    .ok_or_else(invalid_compaction)?,
                extend_digest(saved.covered_digest, &working[previous_len..cutoff])?,
            )
        } else {
            (cutoff, extend_digest([0; 32], &working[..cutoff])?)
        };
        working.splice(..cutoff, replacement.clone());
        request.contents = joined(&pinned, &working);
        let after = self.budget.measure(&request)?;
        after.check()?;
        if after.estimated_input >= before.estimated_input {
            return Err(invalid_compaction());
        }
        tracing::info!(
            covered_count,
            input_before = before.estimated_input,
            input_after = after.estimated_input,
            target_input = after.budget.compaction_target(),
            "prepared scoped context compaction for durable checkpoint persistence"
        );
        Ok((
            request,
            Some(CompactionRecord {
                version: 1,
                definition_digest: self.definition_digest,
                anchor,
                covered_count,
                covered_digest,
                replacement,
            }),
        ))
    }

    async fn summarize(
        &self,
        objective: &Content,
        contents: &[Content],
    ) -> adk_rust::Result<String> {
        let mut events = Vec::with_capacity(contents.len() + 1);
        let mut orientation = Event::new("context-summary-objective");
        "protected-original-user-request".clone_into(&mut orientation.author);
        orientation.set_content(
            Content::new("user")
                .with_text(serde_json::to_string(objective).map_err(|_| invalid_compaction())?),
        );
        events.push(orientation);
        for content in contents {
            let mut event = Event::new("context-summary-source");
            event.author.clone_from(&content.role);
            // ADK's native formatter reads only text. Include structured calls
            // and outcomes explicitly, preserving their IDs and status fields.
            event.set_content(
                Content::new("user")
                    .with_text(serde_json::to_string(content).map_err(|_| invalid_compaction())?),
            );
            events.push(event);
        }
        let summary = self
            .summarizer
            .summarize_events(&events)
            .await?
            .and_then(|event| event.actions.compaction)
            .ok_or_else(invalid_compaction)?;
        let [Part::Text { text }] = summary.compacted_content.parts.as_slice() else {
            return Err(invalid_compaction());
        };
        let source = serde_json::json!({"objective":objective,"history":contents});
        super::context_summary::validate(text, &source)
    }

    pub(super) fn state_value(record: Option<&CompactionRecord>) -> adk_rust::Result<Value> {
        serde_json::to_value(record).map_err(|_| invalid_compaction())
    }

    pub(super) fn committed(&self, record: Option<CompactionRecord>) -> adk_rust::Result<()> {
        *self.record.lock().map_err(|_| invalid_compaction())? = record;
        Ok(())
    }
}

fn joined(pinned: &[Content], working: &[Content]) -> Vec<Content> {
    pinned.iter().chain(working).cloned().collect()
}

fn starts_with(contents: &[Content], prefix: &[Content]) -> bool {
    contents.len() >= prefix.len()
        && contents
            .iter()
            .zip(prefix)
            .all(|(a, b)| a.role == b.role && a.parts == b.parts)
}

fn extend_digest(seed: [u8; 32], contents: &[Content]) -> adk_rust::Result<[u8; 32]> {
    let mut value = seed;
    for content in contents {
        let mut canonical = serde_json::to_value(content).map_err(|_| invalid_compaction())?;
        canonical.sort_all_objects();
        let bytes = serde_json::to_vec(&canonical).map_err(|_| invalid_compaction())?;
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(&value);
        hash.update(&bytes);
        value.copy_from_slice(hash.finish().as_ref());
    }
    Ok(value)
}

/// Only complete call/result groups can leave the model's recent history.
fn complete_prefix(contents: &[Content], preserve: usize) -> adk_rust::Result<usize> {
    let mut pending = HashSet::new();
    let mut cutoff = 0;
    for (index, content) in contents
        .iter()
        .take(contents.len().saturating_sub(preserve))
        .enumerate()
    {
        for part in &content.parts {
            match part {
                Part::FunctionCall { id: Some(id), .. } => {
                    if !pending.insert(id) {
                        return Err(invalid_compaction());
                    }
                }
                Part::FunctionResponse { id: Some(id), .. } => {
                    if !pending.remove(id) {
                        return Err(invalid_compaction());
                    }
                }
                Part::FunctionCall { id: None, .. } | Part::FunctionResponse { id: None, .. } => {
                    return Err(invalid_compaction());
                }
                _ => {}
            }
        }
        if pending.is_empty() {
            cutoff = index + 1;
        }
    }
    Ok(cutoff)
}

fn invalid_compaction() -> AdkError {
    AdkError::new(
        ErrorComponent::Session,
        ErrorCategory::InvalidInput,
        "context_compaction_invalid",
        "The context summary cannot safely replace its source history.",
    )
}

#[cfg(test)]
#[path = "context_compaction_tests.rs"]
mod tests;
