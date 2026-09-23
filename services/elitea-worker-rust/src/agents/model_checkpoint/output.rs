//! Durable answer prefix for automatic child output continuation.

use super::{ModelCheckpointWriter, invalid_checkpoint};
use adk_rust::{AdkError, Content, LlmRequest, Part};
use serde::{Deserialize, Serialize};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(in crate::agents) struct OutputContinuation {
    pub prefix: String,
    pub round: u32,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub repair_used: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub structured_output: bool,
}

impl OutputContinuation {
    pub(in crate::agents) fn validate(&self) -> adk_rust::Result<()> {
        if (self.repair_used && self.round < 2)
            || self.round == 0
            || self.round > crate::agents::assembly::MAX_AGENT_STEP_LIMIT
            || self.prefix.len() > crate::agents::request::MAX_OUTPUT_CONTINUATION_BYTES
        {
            return Err(invalid_checkpoint());
        }
        Ok(())
    }

    pub(in crate::agents) fn anchor(&self) -> &str {
        let mut start = self.prefix.len().saturating_sub(256);
        while !self.prefix.is_char_boundary(start) {
            start += 1;
        }
        &self.prefix[start..]
    }

    pub(in crate::agents) fn request(
        &self,
        mut request: LlmRequest,
        segment: String,
    ) -> LlmRequest {
        let mut parts = take_continuation_parts(&mut request, self.round.saturating_sub(1));
        if !segment.is_empty() {
            parts.push(Part::Text { text: segment });
        }
        if !parts.is_empty() {
            request.contents.push(Content {
                role: "model".into(),
                parts,
            });
        }
        let mut prompt = if self.prefix.is_empty() {
            "The previous response exhausted its output allowance before producing visible text. Complete the original task now. Return the answer without referring to this retry.".to_owned()
        } else {
            format!(
                "The previous answer reached its output allowance. Complete only the missing portion of the original task. Return only the exact anchor below followed immediately by new text. Copy the anchor character-for-character, decoded from JSON, before continuing. The anchor can start or end inside a word; do not complete, skip, revise, or explain the anchor. Preserve whitespace. Do not add a preamble or a new tool call. Exact anchor: {}",
                serde_json::json!(self.anchor())
            )
        };
        if !self.prefix.is_empty() {
            prompt.push_str(" If the original answer is JSON, the schema applies to the joined answer, not this fragment. Do not restart the JSON object or wrap the fragment in quotes or a code fence. Preserve literal JSON escape sequences in the anchor and in new string content; for example, a backslash followed by n must stay those two characters, not become a newline. Close the existing JSON value only when the original task is complete.");
        }
        if self.repair_used {
            prompt.push_str(" The previous continuation was rejected because it did not preserve this exact boundary. Its text was discarded. Begin with the exact anchor, then complete only the remaining original task; do not restart the answer or explain this repair.");
        }
        request
            .contents
            .push(Content::new("user").with_text(prompt));
        // A continuation may start inside a JSON string. The ADK agent validates
        // the joined answer; constraining each fragment to a full object conflicts
        // with the exact accepted boundary. Summary models never use this loop.
        if !self.prefix.is_empty()
            && let Some(config) = &mut request.config
            && let Some(schema) = config.response_schema.take()
        {
            scope_joined_schema_instruction(&mut request.contents, &schema);
        }
        request.previous_response_id = None;
        request
    }
}

// Match only ADK's generated schema instruction, not arbitrary user text.
fn scope_joined_schema_instruction(contents: &mut [Content], schema: &serde_json::Value) {
    let generated = format!(
        "You MUST respond with valid JSON conforming to this schema: {schema}. Do not include any text outside the JSON object."
    );
    for content in contents {
        if content.role == "user"
            && let [Part::Text { text }] = content.parts.as_mut_slice()
            && *text == generated
        {
            *text = format!(
                "The complete joined answer must conform to this JSON schema: {schema}. For output continuation, return the requested exact anchor and missing fragment only. The fragment itself is not a standalone JSON object; do not wrap or restart it."
            );
        }
    }
}

// Replace only this loop's protocol messages. Keep unrelated history and any
// compacted summary intact; never reinsert the full external answer prefix.
fn take_continuation_parts(request: &mut LlmRequest, rounds: u32) -> Vec<Part> {
    let mut segments = Vec::new();
    for _ in 0..rounds {
        let protocol = request.contents.last().is_some_and(|content| {
            content.role == "user" && matches!(content.parts.as_slice(), [Part::Text { text }]
                if text.starts_with("The previous answer reached its output allowance.")
                || text.starts_with("The answer reached its output allowance.")
                || text.starts_with("The previous response exhausted its output allowance before producing visible text."))
        });
        if !protocol {
            break;
        }
        request.contents.pop();
        if request
            .contents
            .last()
            .is_some_and(|content| content.role == "model")
        {
            if let Some(content) = request.contents.pop() {
                segments.push(content.parts);
            }
        } else {
            break;
        }
    }
    segments.into_iter().rev().flatten().collect()
}

impl ModelCheckpointWriter {
    pub(in crate::agents) fn output_continuation(
        &self,
    ) -> adk_rust::Result<Option<OutputContinuation>> {
        self.output_continuation
            .lock()
            .map(|state| state.clone())
            .map_err(|_| invalid_checkpoint())
    }

    pub(in crate::agents) fn set_output_continuation(
        &self,
        state: Option<OutputContinuation>,
    ) -> adk_rust::Result<()> {
        if let Some(state) = &state {
            state.validate()?;
        }
        *self
            .output_continuation
            .lock()
            .map_err(|_| invalid_checkpoint())? = state;
        Ok(())
    }
}

pub(in crate::agents) fn exhausted() -> AdkError {
    AdkError::new(
        adk_rust::ErrorComponent::Model,
        adk_rust::ErrorCategory::Internal,
        "model.output_continuation_failed",
        "The child model could not complete its answer within the continuation contract.",
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn request() -> LlmRequest {
        serde_json::from_value(serde_json::json!({"model":"fixture","contents":[{"role":"user","parts":[{"text":"Original task"}]}]})).unwrap()
    }

    #[test]
    fn repeated_continuation_keeps_one_answer_and_one_protocol_prompt() {
        let mut request = request();
        for round in 1..=3 {
            let state = OutputContinuation {
                prefix: "accepted ".repeat(round as usize),
                round,
                repair_used: false,
                structured_output: false,
            };
            request = state.request(request, "accepted ".into());
            assert_eq!(request.contents.len(), 3);
            assert_eq!(
                serde_json::to_value(&request.contents[0]).unwrap(),
                serde_json::to_value(Content::new("user").with_text("Original task")).unwrap()
            );
            assert_eq!(request.contents[1].parts.len(), round as usize);
            assert_eq!(request.contents[2].role, "user");
        }
    }

    #[test]
    fn continuation_does_not_restore_output_removed_by_compaction() {
        let first = OutputContinuation {
            prefix: "old output".into(),
            round: 1,
            repair_used: false,
            structured_output: false,
        };
        let mut request = first.request(request(), "old output".into());
        request.contents.remove(1);
        request.contents[0] = Content::new("user").with_text("Verified compacted history");
        let next = OutputContinuation {
            prefix: "old output new output".into(),
            round: 2,
            repair_used: false,
            structured_output: false,
        };
        let request = next.request(request, " new output".into());
        assert_eq!(request.contents.len(), 3);
        assert_eq!(
            serde_json::to_value(&request.contents[0]).unwrap(),
            serde_json::to_value(Content::new("user").with_text("Verified compacted history"))
                .unwrap()
        );
        assert_eq!(
            serde_json::to_value(&request.contents[1]).unwrap(),
            serde_json::to_value(Content::new("model").with_text(" new output")).unwrap()
        );
    }
}
