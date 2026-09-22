//! Continue an output-limited child within its existing model/checkpoint scope.

use super::{ScopedModelCheckpoint, invalid_scope};
use crate::agents::model_checkpoint::output::{OutputContinuation, exhausted};
use crate::agents::request::MAX_OUTPUT_CONTINUATION_BYTES;
use adk_rust::futures::StreamExt as _;
use adk_rust::{
    BeforeModelResult, Content, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part,
};
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
    *scope
        .output_completion
        .lock()
        .map_err(|_| invalid_scope())? = None;
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
            let mut responses = inner.generate_content(request.clone(), stream).await?;
            while let Some(response) = responses.next().await {
                let mut response = response?;
                if response.error_code.is_some() || response.error_message.is_some() || response.interrupted {
                    yield response;
                    return;
                }
                if let Some(content) = &mut response.content {
                    for part in &mut content.parts {
                        match part {
                            Part::Text { text } => {
                                let consumed = text.len() - seam.accept(text)?.len();
                                if consumed != 0 { text.drain(..consumed); }
                                extend(&mut segment, text)?;
                            }
                            Part::FunctionCall { .. } | Part::FunctionResponse { .. } => {
                                has_tools = true;
                                if state.is_some() { Err(exhausted())?; }
                            }
                            _ => {}
                        }
                    }
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
            let mut terminal = terminal.ok_or_else(exhausted)?;
            if !seam.finished() || (state.is_some() && segment.is_empty()) { Err(exhausted())?; }
            if terminal.finish_reason != Some(FinishReason::MaxTokens) {
                if !has_tools && matches!(terminal.finish_reason, None | Some(FinishReason::Stop)) {
                    extend(&mut prefix, &segment)?;
                    save_completion(&scope.output_completion, prefix)?;
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
            let next = OutputContinuation { prefix: prefix.clone(), round };
            request = next.request(request, segment);
            writer.checkpoint.set_output_continuation(Some(next.clone()))?;
            // The prefix and exact next request commit together before dispatch.
            request = match writer.checkpoint.before_model(
                writer.identity.clone(), &writer.invocation_id, &writer.agent_name, request,
            ).await? {
                BeforeModelResult::Continue(prepared) => prepared,
                BeforeModelResult::Skip(_) => Err(exhausted())?,
            };
            state = Some(next);
            terminal.turn_complete = false;
            terminal.finish_reason = None;
            terminal.partial = true;
            yield terminal;
        }
    }))
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
    matched: usize,
}
impl Seam {
    fn new(anchor: String) -> Self {
        Self { anchor, matched: 0 }
    }
    fn accept<'a>(&mut self, text: &'a str) -> adk_rust::Result<&'a str> {
        let remaining = &self.anchor.as_bytes()[self.matched..];
        let count = remaining.len().min(text.len());
        if remaining[..count] != text.as_bytes()[..count] {
            tracing::warn!(
                event = "nested_output_anchor_mismatch",
                matched_bytes = self.matched,
                anchor_bytes = self.anchor.len(),
                incoming_bytes = text.len(),
                "The child continuation did not preserve its accepted boundary"
            );
            return Err(exhausted());
        }
        self.matched += count;
        // Both strings are valid UTF-8; equality makes this a character boundary.
        Ok(&text[count..])
    }
    fn finished(&self) -> bool {
        self.matched == self.anchor.len()
    }
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

fn save_completion(target: &Mutex<Option<String>>, text: String) -> adk_rust::Result<()> {
    *target.lock().map_err(|_| invalid_scope())? = Some(text);
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
        assert!(seam.finished());
    }

    #[test]
    fn incomplete_or_different_anchor_never_becomes_a_valid_seam() {
        let mut seam = Seam::new("exact boundary".into());
        assert_eq!(seam.accept("exact ").unwrap(), "");
        assert!(!seam.finished());
        assert!(seam.accept("different").is_err());
    }

    #[test]
    fn replay_chunks_preserve_utf8_and_full_output() {
        let text = "🦀".repeat(5000);
        let parts: Vec<_> = chunks(&text).collect();
        assert!(parts.iter().all(|part| part.len() <= 8192));
        assert_eq!(parts.concat(), text);
    }
}
