//! The grounding indicator of a run's final answer (ADR-0043 decision 5,
//! #1500).
//!
//! The root projector builds the indicator when it builds the final message:
//! it takes the source count that the agent's last model call recorded
//! (`agents::grounding_model`) and reads the final answer text
//! ([`elitea_agent_runtime::grounding::indicator`]). The indicator rides the
//! final message as `response_metadata.grounding`: Main stores it in the
//! message's meta and the chat shows it. The projector also records one step
//! (kind `output`, name [`STEP_NAME`]) with the counts and without the
//! answer text.
//!
//! There is no indicator when the agent does not cite, when the run has no
//! step recorder, or when the model saw no source (plain chat without tools):
//! absent, not zero.

use chrono::{DateTime, Utc};
use serde_json::{Value, json};

use super::AgentEventProjector;
use crate::protocol::elitea::runtime::v1::RunStepKindV1;
use crate::protocol::run_steps::StepDraft;

/// The name of a grounding step.
pub(crate) const STEP_NAME: &str = "grounding_check";

impl AgentEventProjector {
    /// The grounding indicator of the root's final answer `content`, as
    /// response metadata; `Value::Null` when there is none.
    pub(super) fn grounding_indicator(&self, content: &str, occurred_at: DateTime<Utc>) -> Value {
        let Some(recorder) = self
            .run_steps
            .as_ref()
            .filter(|sink| sink.root)
            .map(|sink| &sink.recorder)
        else {
            return Value::Null;
        };
        let Some(sources) = crate::agents::grounding_model::take_sources(recorder.execution_id())
        else {
            return Value::Null;
        };
        let Some(indicator) = elitea_agent_runtime::grounding::indicator(content, sources) else {
            return Value::Null;
        };
        recorder.record(
            StepDraft::new(
                RunStepKindV1::Output,
                STEP_NAME,
                occurred_at.timestamp_millis(),
            )
            .summary("sources_available", json!(indicator.sources_available))
            .summary("sources_cited", json!(indicator.sources_cited))
            .summary("claim_spans", json!(indicator.claim_spans))
            .summary("uncited_spans", json!(indicator.uncited_spans))
            .summary("invalid_markers", json!(indicator.invalid_markers)),
        );
        indicator.to_value()
    }
}

#[cfg(test)]
mod tests {
    use adk_rust::{Content, Event, FinishReason, Part};
    use chrono::{TimeZone, Utc};
    use serde_json::{Value, json};

    use super::super::{
        AgentEventProjectionContext, AgentEventProjector, CompletedAgentBrowserOutput,
    };
    use crate::protocol::node_event::encode_current_node_event_json;
    use crate::protocol::run_steps::RunStepRecorder;

    const ANSWER: &str = "The build is green [1]. The deploy failed.";

    fn full_message(execution_id: &str, sources: Option<usize>) -> Value {
        let at = Utc
            .with_ymd_and_hms(2026, 11, 2, 12, 0, 0)
            .single()
            .expect("timestamp");
        let mut projector =
            AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({})))
                .expect("projector");
        projector.attach_run_steps(RunStepRecorder::new(execution_id));
        if let Some(sources) = sources {
            crate::agents::grounding_model::record_sources(execution_id, sources);
        }
        projector.start(at).expect("start");
        let mut event = Event::with_id("llm-1", "invocation-1");
        event.timestamp = at;
        event.author = "root-agent".to_owned();
        event.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::Text {
                text: ANSWER.to_owned(),
            }],
        });
        event.llm_response.turn_complete = true;
        event.llm_response.finish_reason = Some(FinishReason::Stop);
        projector.project(&event).expect("turn");
        projector
            .finish_after_eos(CompletedAgentBrowserOutput::fixture(ANSWER), at)
            .expect("terminal")
            .into_iter()
            .map(|event| {
                serde_json::from_slice::<Value>(
                    &encode_current_node_event_json(&event).expect("event JSON"),
                )
                .expect("JSON")
            })
            .find(|event| event["type"] == "full_message")
            .expect("full_message")
    }

    #[test]
    fn the_final_message_carries_the_indicator_of_a_citing_agent() {
        let full = full_message("grounding-events-cited", Some(2));
        assert_eq!(
            full["response_metadata"]["grounding"],
            json!({
                "version": 1,
                "sources_available": 2,
                "sources_cited": 1,
                "claim_spans": 2,
                "uncited_spans": 1,
                "uncited": [{"start": 24, "end": 42}],
                "invalid_markers": 0,
            })
        );
    }

    #[test]
    fn no_citation_setting_or_no_source_means_no_indicator() {
        let off = full_message("grounding-events-off", None);
        assert!(off["response_metadata"].get("grounding").is_none());
        let empty = full_message("grounding-events-empty", Some(0));
        assert!(empty["response_metadata"].get("grounding").is_none());
    }
}
