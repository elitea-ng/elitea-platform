//! Durable answer prefix for automatic child output continuation.

use super::{ModelCheckpointWriter, invalid_checkpoint};
use adk_rust::{AdkError, Content, LlmRequest};
use serde::{Deserialize, Serialize};

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(in crate::agents) struct OutputContinuation {
    pub prefix: String,
    pub round: u32,
}

impl OutputContinuation {
    pub(in crate::agents) fn validate(&self) -> adk_rust::Result<()> {
        if self.round == 0
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
        if !segment.is_empty() {
            request
                .contents
                .push(Content::new("model").with_text(segment));
        }
        let prompt = if self.prefix.is_empty() {
            "The previous response exhausted its output allowance before producing visible text. Complete the original task now. Return the answer without referring to this retry.".to_owned()
        } else {
            format!(
                "The answer reached its output allowance. Finish the original task without restarting it. Begin with this exact JSON-encoded tail, decoded as text, then continue after its final character. Preserve whitespace. Do not add a preamble or a new tool call. Tail: {}",
                serde_json::json!(self.anchor())
            )
        };
        request
            .contents
            .push(Content::new("user").with_text(prompt));
        request.previous_response_id = None;
        request
    }
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
