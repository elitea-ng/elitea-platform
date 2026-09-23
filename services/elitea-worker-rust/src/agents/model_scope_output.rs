//! Continue an output-limited child within its existing model/checkpoint scope.

use super::{ScopedModelCheckpoint, invalid_scope};
use crate::agents::model_checkpoint::output::{OutputContinuation, exhausted};
use crate::agents::request::MAX_OUTPUT_CONTINUATION_BYTES;
use adk_rust::futures::StreamExt as _;
use adk_rust::{
    BeforeModelResult, Content, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part,
};
use std::borrow::Cow;
use std::sync::{Arc, Mutex};

pub(super) fn generate(
    scope: Arc<ScopedModelCheckpoint>,
    inner: Arc<dyn Llm>,
    mut request: LlmRequest,
    stream: bool,
    max_model_turns: u32,
) -> adk_rust::Result<LlmResponseStream> {
    let writer = scope.writer.get().ok_or_else(invalid_scope)?;
    let saved = writer.checkpoint.output_continuation()?;
    save_completion(&scope.output_completion, None)?;
    if max_model_turns == 0
        || saved
            .as_ref()
            .is_some_and(|state| state.round >= max_model_turns)
    {
        return Err(exhausted());
    }
    let inner = writer.checkpoint.delegation_model(inner);
    Ok(Box::pin(async_stream::try_stream! {
        let writer = scope.writer.get().ok_or_else(invalid_scope)?;
        let mut state = saved;
        let mut prefix = state.as_ref().map_or_else(String::new, |state| state.prefix.clone());
        // Replay accepted output from durable state, never another provider call.
        for text in chunks(&prefix) {
            yield LlmResponse { content: Some(Content::new("model").with_text(text)), partial: true, ..LlmResponse::default() };
        }
        loop {
            let anchor = state.as_ref().map_or("", OutputContinuation::anchor).to_owned();
            let mut seam = Seam::new(anchor);
            let mut segment = String::new();
            let mut terminal = None;
            let mut has_tools = false;
            let mut invalid_boundary = false;
            let mut responses = inner.generate_content(request.clone(), stream).await?;
            while let Some(response) = responses.next().await {
                let mut response = response?;
                if response.error_code.is_some() || response.error_message.is_some() || response.interrupted {
                    yield response;
                    return;
                }
                match rewrite_content(&mut response, &mut seam, &mut segment, state.is_some()) {
                    Ok(tools) => has_tools |= tools,
                    Err(error) if error.code == "model.output_continuation_boundary" => {
                        invalid_boundary = true;
                        break;
                    }
                    Err(error) => Err(error)?,
                }
                let ended = response.turn_complete || response.finish_reason.is_some();
                if ended {
                    if response.finish_reason == Some(FinishReason::MaxTokens) {
                        let content = response.content.take();
                        if content.is_some() {
                            yield LlmResponse { content, partial: true, ..LlmResponse::default() };
                        }
                    }
                    terminal = Some(response);
                    break;
                }
                response.partial = true;
                yield response;
            }
            drop(responses);
            let boundary_tail = if invalid_boundary { None } else { seam.finish_or_repair()? };
            let Some(boundary_tail) = boundary_tail else {
                let (prepared, next) = prepare_repair(&scope, request, state.as_ref(), max_model_turns).await?;
                request = prepared;
                state = Some(next);
                // A yield after the durable boundary allows recovery without
                // releasing any bytes from the rejected provider result.
                yield LlmResponse { partial: true, ..LlmResponse::default() };
                continue;
            };
            let mut terminal = terminal.ok_or_else(exhausted)?;
            if !boundary_tail.is_empty() {
                extend(&mut segment, &boundary_tail)?;
                yield LlmResponse { content: Some(Content::new("model").with_text(boundary_tail)), partial: true, ..LlmResponse::default() };
            }
            if state.is_some() && segment.is_empty() { Err(exhausted())?; }
            if terminal.finish_reason != Some(FinishReason::MaxTokens) {
                if !has_tools && matches!(terminal.finish_reason, None | Some(FinishReason::Stop)) {
                    extend(&mut prefix, &segment)?;
                    save_completion(&scope.output_completion, Some(prefix))?;
                }
                writer.checkpoint.set_output_continuation(None)?;
                terminal.turn_complete = true;
                terminal.partial = false;
                yield terminal;
                return;
            }
            if has_tools { Err(exhausted())?; }
            extend(&mut prefix, &segment)?;
            let round = state.as_ref().map_or(1, |state| state.round.saturating_add(1));
            if round >= max_model_turns { Err(exhausted())?; }
            let next = OutputContinuation {
                prefix: prefix.clone(), round,
                repair_used: state.as_ref().is_some_and(|state| state.repair_used),
            };
            request = prepare_next(&scope, request, segment, &next).await?;
            state = Some(next);
            terminal.turn_complete = false;
            terminal.finish_reason = None;
            terminal.partial = true;
            yield terminal;
        }
    }))
}

fn rewrite_content(
    response: &mut LlmResponse,
    seam: &mut Seam,
    segment: &mut String,
    is_continuation: bool,
) -> adk_rust::Result<bool> {
    let mut has_tools = false;
    if let Some(content) = &mut response.content {
        for part in &mut content.parts {
            match part {
                Part::Text { text } => {
                    seam.rewrite(text)?;
                    extend(segment, text)?;
                }
                Part::FunctionCall { .. } | Part::FunctionResponse { .. } => {
                    if is_continuation {
                        return Err(exhausted());
                    }
                    has_tools = true;
                }
                _ => {}
            }
        }
    }
    Ok(has_tools)
}

async fn prepare_repair(
    scope: &ScopedModelCheckpoint,
    request: LlmRequest,
    previous: Option<&OutputContinuation>,
    max_model_turns: u32,
) -> adk_rust::Result<(LlmRequest, OutputContinuation)> {
    let previous = previous.ok_or_else(exhausted)?;
    let round = previous.round.saturating_add(1);
    if previous.repair_used || round >= max_model_turns {
        return Err(exhausted());
    }
    if let Some(completion) = &scope.completion {
        completion.discard_unaccepted()?;
    }
    let next = OutputContinuation {
        prefix: previous.prefix.clone(),
        round,
        repair_used: true,
    };
    let prepared = prepare_next(scope, request, String::new(), &next).await?;
    tracing::info!(
        event = "nested_output_boundary_repair_checkpointed",
        round,
        "One continuation boundary repair was durably prepared"
    );
    Ok((prepared, next))
}

async fn prepare_next(
    scope: &ScopedModelCheckpoint,
    request: LlmRequest,
    segment: String,
    next: &OutputContinuation,
) -> adk_rust::Result<LlmRequest> {
    let writer = scope.writer.get().ok_or_else(invalid_scope)?;
    let request = next.request(request, segment);
    writer
        .checkpoint
        .set_output_continuation(Some(next.clone()))?;
    // Prefix, repair allowance, and exact request commit before dispatch.
    match writer
        .checkpoint
        .before_model(
            writer.identity.clone(),
            &writer.invocation_id,
            &writer.agent_name,
            request,
        )
        .await?
    {
        BeforeModelResult::Continue(prepared) => Ok(prepared),
        BeforeModelResult::Skip(_) => Err(exhausted()),
    }
}

fn extend(target: &mut String, text: &str) -> adk_rust::Result<()> {
    if target.len().saturating_add(text.len()) > MAX_OUTPUT_CONTINUATION_BYTES {
        return Err(exhausted());
    }
    target.push_str(text);
    Ok(())
}

fn chunks(mut text: &str) -> impl Iterator<Item = &str> {
    std::iter::from_fn(move || {
        if text.is_empty() {
            return None;
        }
        let mut end = text.len().min(8192);
        while !text.is_char_boundary(end) {
            end -= 1;
        }
        let (chunk, rest) = text.split_at(end);
        text = rest;
        Some(chunk)
    })
}

struct Seam {
    anchor: String,
    pending: String,
    resolved: bool,
}
impl Seam {
    fn new(anchor: String) -> Self {
        let resolved = anchor.is_empty();
        Self {
            anchor,
            pending: String::new(),
            resolved,
        }
    }
    fn rewrite(&mut self, text: &mut String) -> adk_rust::Result<()> {
        match self.accept(text)? {
            Cow::Borrowed(accepted) => {
                let consumed = text.len() - accepted.len();
                if consumed != 0 {
                    text.drain(..consumed);
                }
            }
            Cow::Owned(accepted) => *text = accepted,
        }
        Ok(())
    }
    fn accept<'a>(&mut self, text: &'a str) -> adk_rust::Result<Cow<'a, str>> {
        if self.resolved {
            return Ok(Cow::Borrowed(text));
        }
        // Wait for enough bytes to prefer the longest verified overlap.
        // Do not release any unverified response prefix to the parent.
        extend(&mut self.pending, text)?;
        if self.pending.len() < self.anchor.len() {
            return Ok(Cow::Borrowed(""));
        }
        self.resolve().map(Cow::Owned)
    }
    fn finish_or_repair(&mut self) -> adk_rust::Result<Option<String>> {
        match self.finish() {
            Ok(text) => Ok(Some(text)),
            Err(error) if error.code == "model.output_continuation_boundary" => Ok(None),
            Err(error) => Err(error),
        }
    }
    fn finish(&mut self) -> adk_rust::Result<String> {
        if self.resolved {
            return Ok(String::new());
        }
        self.resolve()
    }
    fn resolve(&mut self) -> adk_rust::Result<String> {
        let Some(overlap) = exact_overlap(&self.anchor, &self.pending) else {
            tracing::warn!(
                event = "nested_output_anchor_mismatch",
                anchor_bytes = self.anchor.len(),
                incoming_bytes = self.pending.len(),
                "The child continuation did not preserve its accepted boundary"
            );
            return Err(adk_rust::AdkError::new(
                adk_rust::ErrorComponent::Model,
                adk_rust::ErrorCategory::Internal,
                "model.output_continuation_boundary",
                "The continuation did not preserve the accepted answer boundary.",
            ));
        };
        self.resolved = true;
        let mut text = std::mem::take(&mut self.pending);
        text.drain(..overlap);
        Ok(text)
    }
}

fn exact_overlap(anchor: &str, incoming: &str) -> Option<usize> {
    if incoming.starts_with(anchor) {
        return Some(anchor.len());
    }
    // Match exact suffixes only. Never search past an incoming preamble,
    // rewrite accepted text, or join different fragments of a word.
    for (start, first) in anchor.char_indices().skip(1) {
        let suffix = &anchor[start..];
        if suffix.len() < 32 {
            break;
        }
        let previous = anchor[..start].chars().next_back()?;
        let joins_word = previous.is_alphanumeric() && first.is_alphanumeric();
        if !joins_word && incoming.starts_with(suffix) {
            let last = suffix.chars().next_back()?;
            let next = incoming[suffix.len()..].chars().next();
            if !last.is_alphanumeric() || next.is_none_or(|next| !next.is_alphanumeric()) {
                return Some(suffix.len());
            }
        }
    }
    let line = anchor.rsplit('\n').next()?;
    if line.chars().filter(|c| c.is_alphanumeric()).count() >= 4 && incoming.starts_with(line) {
        return Some(line.len());
    }
    None
}

pub(super) struct Completion {
    pub provider: Option<Arc<dyn crate::agents::session::DurableModelCompletion>>,
    pub output: Arc<Mutex<Option<String>>>,
}
impl crate::agents::session::DurableModelCompletion for Completion {
    fn snapshot(&self) -> adk_rust::Result<Option<String>> {
        if let Some(output) = self.output.lock().map_err(|_| invalid_scope())?.clone() {
            return Ok(Some(output));
        }
        self.provider
            .as_ref()
            .map_or(Ok(None), |provider| provider.snapshot())
    }
}

fn save_completion(target: &Mutex<Option<String>>, text: Option<String>) -> adk_rust::Result<()> {
    *target.lock().map_err(|_| invalid_scope())? = text;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn seam_accepts_unicode_tail_split_across_chunks() {
        let mut seam = Seam::new("кінець 🦀".into());
        assert_eq!(seam.accept("кі").unwrap(), "");
        assert_eq!(seam.accept("нець ").unwrap(), "");
        assert_eq!(seam.accept("🦀 ending").unwrap(), " ending");
        assert!(seam.finish().unwrap().is_empty());
    }

    #[test]
    fn incomplete_or_different_anchor_never_becomes_a_valid_seam() {
        let mut seam = Seam::new("exact boundary".into());
        assert_eq!(seam.accept("exact ").unwrap(), "");
        assert!(seam.finish().is_err());
        assert!(seam.accept("different").is_err());
    }

    #[test]
    fn shorter_exact_line_overlap_preserves_new_text_across_chunks() {
        let mut seam = Seam::new(
            "Earlier accepted record.\nRECORD 081: Cedar archive verification remains complete."
                .into(),
        );
        assert_eq!(seam.accept("RECORD 081: Cedar archive ").unwrap(), "");
        assert_eq!(
            seam.accept("verification remains complete.\nRECORD 082")
                .unwrap(),
            ""
        );
        assert_eq!(seam.finish().unwrap(), "\nRECORD 082");
        assert_eq!(seam.accept(": new content").unwrap(), ": new content");
    }

    #[test]
    fn repeated_suffix_prefers_full_anchor_over_shorter_overlap() {
        let line = "RECORD: The exact repeated line remains accepted.\n";
        let anchor = line.repeat(3);
        let mut seam = Seam::new(anchor.clone());
        assert_eq!(seam.accept(line).unwrap(), "");
        assert_eq!(seam.accept(line).unwrap(), "");
        assert_eq!(seam.accept(&format!("{line}new text")).unwrap(), "new text");
        assert!(seam.finish().unwrap().is_empty());
    }

    #[test]
    fn suffix_validation_rejects_preambles_edits_and_midword_overlaps() {
        let anchor =
            "Earlier accepted text.\nRECORD 081: Cedar archive verification remains complete.";
        for incoming in [
            "Here is the continuation: RECORD 081: Cedar archive verification remains complete.",
            "RECORD 081: Cedar archive verification is different.",
            "RECORD 082: Cedar archive verification remains complete.",
            "ar archive verification remains complete.",
        ] {
            assert_eq!(exact_overlap(anchor, incoming), None);
        }
    }

    #[test]
    fn replay_chunks_preserve_utf8_and_full_output() {
        let text = "🦀".repeat(5000);
        let parts: Vec<_> = chunks(&text).collect();
        assert!(parts.iter().all(|part| part.len() <= 8192));
        assert_eq!(parts.concat(), text);
    }
}
