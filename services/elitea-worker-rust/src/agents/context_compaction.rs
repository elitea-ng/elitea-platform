//! Persist scoped summary coverage with the pending model request.

use std::collections::HashSet;
use std::sync::{Arc, Mutex};

use adk_rust::agent::LlmEventSummarizer;
use adk_rust::futures::StreamExt as _;
use adk_rust::session::Session;
use adk_rust::{
    AdkError, BaseEventsSummarizer, Content, ErrorCategory, ErrorComponent, Event, Llm, LlmRequest,
    Part,
};
use ring::digest;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::context_budget::{ModelRequestBudget, RequestContextUsage};
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
    repair_summarizer: Arc<dyn BaseEventsSummarizer>,
    model: Arc<dyn Llm>,
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
            LlmEventSummarizer::new(model.clone())
                .with_prompt_template(super::context_summary::prompt(&plan.summary_instructions)),
        );
        let repair_summarizer = Arc::new(
            LlmEventSummarizer::new(model.clone()).with_prompt_template(format!(
                "{}\nThe previous candidate failed validation. Correct it using only the original source records. The final validation-feedback record contains the rejected candidate and a static failure code, not additional source evidence. Return the complete corrected record. Each evidence_refs entry must equal a references.value, not a label. Every referenced value must exist in the original source. Preserve valid facts; do not invent references to satisfy validation. When correction_scope is evidence_refs_only, only change completed_work evidence_refs arrays. Select exact entries from allowed_evidence_refs. Use an empty array when no allowed entry supports the result. Preserve every other field exactly. Original bulk history is omitted for this scoped correction; the platform already checked the allowed reference values.",
                super::context_summary::prompt(&plan.summary_instructions)
            )),
        );
        Ok(Self {
            plan,
            budget,
            summarizer,
            repair_summarizer,
            model,
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
        F: FnOnce(RequestContextUsage) -> Fut + Send,
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
        // Recent-message count is a preference. Large records can otherwise
        // consume most of the fresh window even after a successful summary.
        let mut cutoff = complete_prefix(&working, self.plan.preserve_recent_messages)?;
        let summary_reservation = (super::context_summary::MAX_SUMMARY_BYTES / 4) as u64;
        for preserve in (1..=self.plan.preserve_recent_messages.min(working.len())).rev() {
            let candidate = complete_prefix(&working, preserve)?;
            if candidate < cutoff {
                continue;
            }
            cutoff = candidate;
            request.contents = joined(&pinned, &working[cutoff..]);
            let retained = self.budget.measure(&request)?;
            if retained.estimated_input.saturating_add(summary_reservation)
                <= before.budget.compaction_target()
            {
                break;
            }
        }
        request.contents = joined(&pinned, &working);
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
        before_summary(before).await?;
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
        let mut remaining = super::context_summary::MAX_BATCH_ATTEMPTS;
        self.summarize_bounded(objective, contents, &mut remaining, None)
            .await
    }

    /// Admission failures split source records. Partial summaries are never committed.
    fn summarize_bounded<'a>(
        &'a self,
        objective: &'a Content,
        contents: &'a [Content],
        remaining: &'a mut u32,
        original_source: Option<&'a Value>,
    ) -> adk_rust::futures::future::BoxFuture<'a, adk_rust::Result<String>> {
        Box::pin(async move {
            *remaining = remaining.checked_sub(1).ok_or_else(summary_capacity)?;
            match self
                .summarize_once(objective, contents, original_source)
                .await
            {
                Ok(summary) => Ok(summary),
                Err(error)
                    if matches!(
                        error.code,
                        "context_budget_exceeded" | "model_request_bytes_exceeded"
                    ) =>
                {
                    if contents.len() < 2 {
                        return Err(summary_capacity());
                    }
                    let (earlier, later) = contents.split_at(contents.len() / 2);
                    let earlier = self
                        .summarize_bounded(objective, earlier, remaining, original_source)
                        .await?;
                    let later = self
                        .summarize_bounded(objective, later, remaining, original_source)
                        .await?;
                    let partials = [
                        Content::new("user").with_text(format!("Validated earlier source summary:\n{earlier}")),
                        Content::new("user").with_text(format!("Validated later source summary. Later corrections take precedence:\n{later}")),
                    ];
                    // Validate merges against original records during repair, not
                    // only after the correction opportunity has already passed.
                    let source = serde_json::json!({"objective":objective,"history":contents});
                    self.summarize_bounded(
                        objective,
                        &partials,
                        remaining,
                        Some(original_source.unwrap_or(&source)),
                    )
                    .await
                }
                Err(error) => Err(error),
            }
        })
    }

    async fn summarize_once(
        &self,
        objective: &Content,
        contents: &[Content],
        original_source: Option<&Value>,
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
        let source = original_source.unwrap_or(&source);
        match super::context_summary::validate(text, source) {
            Ok(validated) => Ok(validated),
            Err(error) => {
                // One correction attempt. Never commit rejected text or use it
                // as evidence when validating the replacement.
                tracing::info!(
                    validation_code = error.code,
                    "correcting invalid context summary"
                );
                let mut feedback = Event::new("context-summary-validation");
                "validation-feedback".clone_into(&mut feedback.author);
                let evidence_only = error.code == "context_summary_evidence";
                if evidence_only {
                    return self.correct_evidence(text, source).await;
                }
                let feedback_value = if error.code == "context_summary_reference" {
                    super::context_summary::reference_correction_input(text, source)?
                } else {
                    serde_json::json!({"validation_code": error.code, "rejected_candidate": text})
                };
                feedback.set_content(Content::new("user").with_text(feedback_value.to_string()));
                events.push(feedback);
                let corrected = self
                    .repair_summarizer
                    .summarize_events(&events)
                    .await?
                    .and_then(|event| event.actions.compaction)
                    .ok_or_else(invalid_compaction)?;
                let [
                    Part::Text {
                        text: corrected_text,
                    },
                ] = corrected.compacted_content.parts.as_slice()
                else {
                    return Err(invalid_compaction());
                };
                match super::context_summary::validate(corrected_text, source) {
                    Err(error) if error.code == "context_summary_evidence" => {
                        // Reference repair can leave dangling links. One final
                        // evidence-only correction cannot rewrite accepted facts.
                        self.correct_evidence(corrected_text, source).await
                    }
                    result => result,
                }
            }
        }
    }

    async fn correct_evidence(&self, text: &str, source: &Value) -> adk_rust::Result<String> {
        let feedback = super::context_summary::evidence_correction_input(text)?;
        let request = LlmRequest {
            model: self.model.name().to_owned(),
            contents: vec![Content::new("user").with_text(format!(
                "Repair only completed_work evidence_refs. Select exact allowed_evidence_refs values, or use an empty array. Preserve all other fields and work order. Return the complete corrected record.\n{feedback}"
            ))],
            config: Some(adk_rust::GenerateContentConfig {
                response_schema: Some(super::context_summary::evidence_response_schema(text)?),
                ..adk_rust::GenerateContentConfig::default()
            }),
            ..LlmRequest::new(self.model.name(), Vec::new())
        };
        let mut responses = self.model.generate_content(request, false).await?;
        let response = responses.next().await.ok_or_else(invalid_compaction)??;
        if responses.next().await.is_some() {
            return Err(invalid_compaction());
        }
        let content = response.content.ok_or_else(invalid_compaction)?;
        let [
            Part::Text {
                text: corrected_text,
            },
        ] = content.parts.as_slice()
        else {
            return Err(invalid_compaction());
        };
        super::context_summary::apply_evidence_correction(text, corrected_text, source)
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

fn summary_capacity() -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::InvalidInput,
        "context_summary_capacity",
        "The selected summarization model cannot compact these records within its capacity. Select a larger summarization model.",
    )
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
