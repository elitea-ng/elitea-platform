//! Step records from the projector (ADR-0042 decision 1, #1499).
//!
//! The projector is the one place every model turn, tool call and final
//! answer of a run passes through with its whole context (the toolkit, the
//! masked arguments, the raw result, the usage), for the root agent, its
//! pipeline nodes and its sub-agents alike. It records the `model_call`,
//! `tool_call`, retrieval, delegation, output and output-limit cap steps here,
//! in the order it sees them; the lifecycle records approvals, run-ending
//! caps and errors.

use adk_rust::Event;
use chrono::{DateTime, Utc};
use serde_json::{Value, json};

use super::{ActiveToolCall, AgentEventProjector, pipeline_node_name};
use crate::protocol::elitea::runtime::v1::RunStepKindV1;
use crate::protocol::run_steps::{StepDraft, StepText, retrieval_from_tool, rfc3339_unix_ms};

/// What [`AgentEventProjector::record_model_step`] needs from a closed turn.
pub(super) struct ClosedModelTurn<'a> {
    pub(super) event: &'a Event,
    pub(super) timestamp_start: &'a str,
    pub(super) timestamp_finish: &'a str,
    pub(super) content: &'a str,
    pub(super) output_limited: bool,
}

impl AgentEventProjector {
    /// Record into `recorder` from now on, as the run's root projector:
    /// sub-agent projectors created later share it.
    pub(crate) fn attach_run_steps(
        &mut self,
        recorder: std::sync::Arc<crate::protocol::run_steps::RunStepRecorder>,
    ) {
        self.run_steps = Some(super::RunStepSink {
            recorder,
            root: true,
        });
    }

    /// A sub-agent's projector records into the root's recorder, but never
    /// the run's output.
    pub(super) fn descendant_run_steps(&self) -> Option<super::RunStepSink> {
        self.run_steps.clone().map(|sink| super::RunStepSink {
            root: false,
            ..sink
        })
    }

    /// A model turn closed: one `model_call` step, and a cap step when the
    /// provider stopped it at its output limit.
    ///
    /// Not every function-call event is a provider call. The runtime
    /// synthesizes two kinds, both carrying only function calls and no usage:
    /// a pipeline node's start of a toolkit or saved-application call (no
    /// finish reason; the call itself is the tool or delegation step), and,
    /// in a resumed generation, the replay adapters' re-emission of the calls
    /// the earlier generation's model made (direct HITL, application and
    /// pipeline replay), which that generation already reported. Neither is
    /// recorded as a model call.
    pub(super) fn record_model_step(&self, turn: &ClosedModelTurn<'_>) {
        let Some(recorder) = self.run_steps.as_ref().map(|sink| &sink.recorder) else {
            return;
        };
        let event = turn.event;
        let usage = event.llm_response.usage_metadata.as_ref();
        let calls = event.tool_calls();
        if usage.is_none()
            && turn.content.is_empty()
            && !calls.is_empty()
            && (recorder.is_resumed() || event.llm_response.finish_reason.is_none())
        {
            return;
        }
        let node_id = pipeline_node_name(event)
            .ok()
            .flatten()
            .unwrap_or_default()
            .to_owned();
        let model = self.context.model_name.clone();
        let mut step = StepDraft::new(
            RunStepKindV1::ModelCall,
            model.clone(),
            rfc3339_unix_ms(turn.timestamp_start),
        )
        .summary("model", json!(model))
        .summary(
            "finish_reason",
            event
                .llm_response
                .finish_reason
                .as_ref()
                .map_or(Value::Null, |reason| json!(format!("{reason:?}"))),
        )
        .summary("tool_calls", json!(calls.len()))
        .summary("output_limited", json!(turn.output_limited));
        if let Some(usage) = usage {
            step = step
                .summary("input_tokens", json!(usage.prompt_token_count))
                .summary("output_tokens", json!(usage.candidates_token_count))
                .summary("total_tokens", json!(usage.total_token_count))
                .summary(
                    "cached_input_tokens",
                    usage
                        .cache_read_input_token_count
                        .map_or(Value::Null, |count| json!(count)),
                )
                .summary(
                    "thinking_tokens",
                    usage
                        .thinking_token_count
                        .map_or(Value::Null, |count| json!(count)),
                );
        }
        step.ended_at_unix_ms = rfc3339_unix_ms(turn.timestamp_finish);
        step.node_id.clone_from(&node_id);
        // The answer text and the calls the model asked for; the arguments
        // ride the tool steps, masked.
        step.outputs = Some(StepText::json(&json!({
            "text": turn.content,
            "tool_calls": calls
                .iter()
                .map(|call| json!({"id": call.call_id, "name": call.name}))
                .collect::<Vec<_>>(),
        })));
        let ended = step.ended_at_unix_ms;
        recorder.record(step);
        if turn.output_limited {
            let mut cap = StepDraft::new(RunStepKindV1::Cap, "output_tokens", ended)
                .summary("cap", json!("output_tokens"))
                .summary("model", json!(self.context.model_name));
            cap.node_id = node_id;
            recorder.record(cap);
        }
    }

    /// A tool call completed: a delegation step for a saved agent or
    /// pipeline called as a tool, a retrieval step for a retrieval tool, a
    /// `tool_call` step otherwise. A failed call is the same step with
    /// `"status": "error"`.
    pub(super) fn record_tool_step(
        &self,
        call_id: &str,
        active: &ActiveToolCall,
        response: &Value,
        timestamp_finish: &str,
    ) {
        let Some(recorder) = self.run_steps.as_ref().map(|sink| &sink.recorder) else {
            return;
        };
        let shown = active.display_name.as_deref().unwrap_or(&active.name);
        let error = response
            .as_object()
            .and_then(|value| value.get("error"))
            .and_then(Value::as_str);
        let retrieval = if active.application.is_none() {
            retrieval_from_tool(shown, &active.public_arguments, response)
        } else {
            None
        };
        let (kind, name) = match (&active.application, &retrieval) {
            (Some(application), _) => (RunStepKindV1::Delegation, application.display_name.clone()),
            (None, Some(_)) => (RunStepKindV1::Retrieval, shown.to_owned()),
            (None, None) => (RunStepKindV1::ToolCall, shown.to_owned()),
        };
        let mut step = StepDraft::new(kind, name, rfc3339_unix_ms(&active.timestamp_start))
            .summary("tool_call_id", json!(call_id))
            .summary(
                "status",
                json!(if error.is_some() { "error" } else { "ok" }),
            )
            .summary("error", error.map_or(Value::Null, |error| json!(error)));
        if let Some(application) = active.application.as_ref() {
            step = step
                .summary("agent_type", json!(application.agent_type))
                .summary("tool_name", json!(active.name))
                .summary("model", json!(application.model_name));
        }
        if let Some(toolkit) = active.toolkit.as_ref() {
            step = step
                .summary("toolkit_name", json!(toolkit.name))
                .summary("toolkit_type", json!(toolkit.kind));
            if let Some(id) = toolkit.id {
                step.toolkit_id = id.to_string();
            }
        }
        if let Some(retrieval) = retrieval.as_ref() {
            step = step.summary("documents", json!(retrieval.documents.len()));
        }
        step.ended_at_unix_ms = rfc3339_unix_ms(timestamp_finish);
        step.node_id = active.pipeline_node_name.clone().unwrap_or_default();
        step.inputs = Some(StepText::json(&active.public_arguments));
        step.outputs = Some(StepText::json(response));
        step.retrieval = retrieval;
        recorder.record(step);
    }

    /// The run's final answer: one output step citing the retrieval steps
    /// whose documents it names. Recorded by the root projector only (a
    /// sub-agent's answer is its delegation step's output).
    pub(super) fn record_output_step(
        &self,
        content: &str,
        output_limited: bool,
        occurred_at: DateTime<Utc>,
    ) {
        let Some(recorder) = self
            .run_steps
            .as_ref()
            .filter(|sink| sink.root)
            .map(|sink| &sink.recorder)
        else {
            return;
        };
        let mut step = StepDraft::new(
            RunStepKindV1::Output,
            "output",
            occurred_at.timestamp_millis(),
        )
        .summary("output_limited", json!(output_limited))
        .summary("bytes", json!(content.len()));
        step.outputs = Some(StepText::plain(content));
        recorder.record(step);
    }
}
