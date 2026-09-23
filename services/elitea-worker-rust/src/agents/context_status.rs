//! Public context occupancy: estimates and budget arithmetic, never model content.

use adk_rust::{AdkError, Event};
use serde::{Deserialize, Serialize};

use super::context_budget::RequestContextUsage;

pub(super) const METADATA_KEY: &str = "elitea.context.status.v1";
const MAX_ENCODED_BYTES: usize = 1024;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub(super) enum ContextPhase {
    Measured,
    Compacting,
    Compacted,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub(super) struct ModelContextStatus {
    version: u8,
    pub(super) phase: ContextPhase,
    budget_mode: String,
    total_tokens: u32,
    usable_input_tokens: u32,
    reserved_output_tokens: u32,
    safety_margin_tokens: u32,
    estimated_input_tokens: u64,
    compaction_trigger_tokens: u64,
    compaction_target_tokens: u64,
}

impl ModelContextStatus {
    pub(super) fn new(phase: ContextPhase, usage: RequestContextUsage) -> Self {
        Self {
            version: 1,
            phase,
            budget_mode: usage.budget.mode().to_owned(),
            total_tokens: usage.budget.total_tokens,
            usable_input_tokens: usage.budget.input_limit,
            reserved_output_tokens: usage.budget.output_reservation,
            safety_margin_tokens: usage.budget.margin_tokens,
            estimated_input_tokens: usage.estimated_input,
            compaction_trigger_tokens: usage.budget.compaction_trigger(),
            compaction_target_tokens: usage.budget.compaction_target(),
        }
    }

    pub(super) fn event(&self) -> adk_rust::Result<Event> {
        self.validate()?;
        let mut event = Event::new("model-context-status");
        // This is progress, not a model result or another transcript message.
        // In particular, completion adapters must not fill it with model text.
        event.llm_response.partial = true;
        event.provider_metadata.insert(
            METADATA_KEY.to_owned(),
            serde_json::to_string(self).map_err(|_| invalid_status())?,
        );
        Ok(event)
    }

    pub(super) fn from_event(event: &Event) -> adk_rust::Result<Option<Self>> {
        let Some(encoded) = event.provider_metadata.get(METADATA_KEY) else {
            return Ok(None);
        };
        if encoded.len() > MAX_ENCODED_BYTES
            || event.content().is_some()
            || !event.llm_response.partial
            || event.llm_response.turn_complete
            || !event.actions.state_delta.is_empty()
        {
            return Err(invalid_status());
        }
        let status: Self = serde_json::from_str(encoded).map_err(|_| invalid_status())?;
        status.validate()?;
        Ok(Some(status))
    }

    fn validate(&self) -> adk_rust::Result<()> {
        let input = u64::from(self.usable_input_tokens);
        if self.version != 1
            || !matches!(self.budget_mode.as_str(), "balanced" | "full" | "legacy")
            || input == 0
            || self.reserved_output_tokens == 0
            || self.safety_margin_tokens == 0
            || input + u64::from(self.reserved_output_tokens) + u64::from(self.safety_margin_tokens)
                > u64::from(self.total_tokens)
            || self.compaction_trigger_tokens != (input * 90).div_ceil(100)
            || ![input * 15 / 100, input * 70 / 100].contains(&self.compaction_target_tokens)
            || self.estimated_input_tokens > 9_007_199_254_740_991
            || (self.phase != ContextPhase::Compacting && self.estimated_input_tokens > input)
        {
            return Err(invalid_status());
        }
        Ok(())
    }
}

fn invalid_status() -> AdkError {
    AdkError::agent("The model context status is invalid.")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::context_budget::RequestContextBudget;
    use crate::agents::request::ModelContextLimits;

    pub(super) fn status() -> ModelContextStatus {
        let budget = RequestContextBudget::resolve(
            Some(ModelContextLimits {
                context_window_tokens: 400_000,
                max_output_tokens: 64_000,
                max_input_tokens: None,
                context_window_fallback: false,
                max_output_fallback: false,
            }),
            &serde_json::Map::new(),
            Some(64_000),
        )
        .unwrap()
        .unwrap();
        ModelContextStatus::new(
            ContextPhase::Measured,
            RequestContextUsage {
                budget,
                estimated_input: 100_000,
                request_bytes: 400_000,
                request_byte_limit: 1_048_576,
            },
        )
    }

    #[test]
    fn status_is_content_free_and_reserves_output_inside_the_window() {
        let event = status().event().unwrap();
        let parsed = ModelContextStatus::from_event(&event).unwrap().unwrap();
        assert_eq!(parsed.total_tokens, 272_000);
        assert_eq!(parsed.reserved_output_tokens, 64_000);
        assert_eq!(parsed.usable_input_tokens, 205_280);
        assert_eq!(parsed.compaction_trigger_tokens, 184_752);
        assert!(event.content().is_none());
        assert!(event.actions.state_delta.is_empty());
        assert!(event.provider_metadata[METADATA_KEY].len() <= MAX_ENCODED_BYTES);
    }

    #[test]
    fn rejects_fabricated_capacity_and_mixed_message_content() {
        let mut malformed = status();
        malformed.usable_input_tokens = malformed.total_tokens;
        assert!(malformed.event().is_err());
        let mut event = status().event().unwrap();
        event.set_content(adk_rust::Content::new("model").with_text("PRIVATE_CONTENT"));
        assert!(ModelContextStatus::from_event(&event).is_err());
        let mut event = status().event().unwrap();
        let encoded = event.provider_metadata.get_mut(METADATA_KEY).unwrap();
        *encoded = encoded.replace(
            "\"version\":1",
            "\"version\":1,\"request\":\"PRIVATE_CONTENT\"",
        );
        assert!(ModelContextStatus::from_event(&event).is_err());
    }
}
