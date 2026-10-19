//! Admit a `fanout_wake` continuation (fanout-interrupt-decisions v1.1 §8.1).
//!
//! Main admits a wake for a parked fan-out execution once a decision lands. The
//! wake carries no decision and no interrupt kind: the re-entered fan-out node
//! fetches every decided card itself. This module only proves the input shape;
//! it grants no resume authority. The graph re-enters from the parked frontier
//! that the explicit resume reads (`agents/session.rs` `resolve_pipeline_start`).

use serde::Deserialize;
use serde_json::Value;

use crate::agents::request::{AgentExecutionPayload, UserInput};

/// `payload.meta` member carrying the wake marker.
pub(crate) const FANOUT_WAKE_META_KEY: &str = "fanout_wake_v1";
const FANOUT_WAKE_SCHEMA: &str = "elitea.pipeline.fanout-wake.v1";
/// Meta keys that select another continuation kind; a wake never carries one.
const OTHER_CONTINUATION_META_KEYS: [&str; 2] = [
    super::static_tool_pause::STATIC_TOOL_RESUME_META_KEY,
    "pipeline_static_resume_v1",
];

/// A validated wake marker. It names the parked response and the decision
/// revision that admitted the wake; it carries no decision.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FanoutWake {
    response_message_id: String,
    decision_revision: u64,
}

impl FanoutWake {
    /// `Ok(None)` when the payload carries no wake marker. A marker is admitted
    /// only on an input that is otherwise empty (§8.1); anything else is a
    /// typed refusal, never a fresh turn.
    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Option<Self>, FanoutWakeError> {
        let Some(marker) = payload.meta.get(FANOUT_WAKE_META_KEY) else {
            return Ok(None);
        };
        let wake = Self::parse(marker)?;
        let empty_input = matches!(&payload.user_input, UserInput::Text(text) if text.is_empty());
        if payload.hitl_resume
            || payload.should_continue
            || payload.hitl_action.is_some()
            || payload.hitl_value.is_some()
            || !payload.hitl_decisions.is_empty()
            || !empty_input
            || payload.is_regenerate
            || payload.truncated_content.is_some()
            || payload.checkpoint_id.is_some()
            || payload.parallel_reconcile.is_some()
            || !payload.parallel_terminal_errors.is_empty()
            || OTHER_CONTINUATION_META_KEYS
                .iter()
                .any(|key| payload.meta.contains_key(*key))
        {
            return Err(FanoutWakeError::NotAWake);
        }
        Ok(Some(wake))
    }

    /// Strict `fanout-wake.schema.json`: exactly three members, the schema
    /// constant, a lowercase non-nil UUID and a revision in `1..=i64::MAX`.
    fn parse(value: &Value) -> Result<Self, FanoutWakeError> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Marker {
            schema: String,
            response_message_id: String,
            decision_revision: i64,
        }
        let marker = Marker::deserialize(value).map_err(|_| FanoutWakeError::MalformedMarker)?;
        if marker.schema != FANOUT_WAKE_SCHEMA
            || !valid_response_message_id(&marker.response_message_id)
        {
            return Err(FanoutWakeError::MalformedMarker);
        }
        let decision_revision = u64::try_from(marker.decision_revision)
            .ok()
            .filter(|revision| *revision >= 1)
            .ok_or(FanoutWakeError::MalformedMarker)?;
        Ok(Self {
            response_message_id: marker.response_message_id,
            decision_revision,
        })
    }

    #[allow(
        dead_code,
        reason = "read by the restore fetch when commit 10 composes Parallel"
    )]
    pub(crate) fn response_message_id(&self) -> &str {
        &self.response_message_id
    }

    #[allow(
        dead_code,
        reason = "read by the restore fetch when commit 10 composes Parallel"
    )]
    pub(crate) const fn decision_revision(&self) -> u64 {
        self.decision_revision
    }
}

/// Lowercase canonical UUID text, not the nil UUID (schema pattern).
fn valid_response_message_id(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(index, byte)| match index {
            8 | 13 | 18 | 23 => byte == b'-',
            _ => byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte),
        })
        && value != "00000000-0000-0000-0000-000000000000"
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub(crate) enum FanoutWakeError {
    /// The marker does not match `fanout-wake.schema.json`.
    #[error("graph.fanout.wake_malformed")]
    MalformedMarker,
    /// The marker rides on an input that carries a decision, text or another
    /// continuation kind.
    #[error("graph.fanout.wake_not_empty")]
    NotAWake,
}

impl FanoutWakeError {
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::MalformedMarker => "graph.fanout.wake_malformed",
            Self::NotAWake => "graph.fanout.wake_not_empty",
        }
    }
}

#[cfg(test)]
#[path = "fanout_wake_tests.rs"]
mod tests;
