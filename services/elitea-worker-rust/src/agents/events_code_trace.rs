//! Code progress uses existing tool rows without changing model/tool turn state.
use adk_rust::Event;
use chrono::SecondsFormat;
use serde_json::{Value, json};

use super::{
    AgentEventProjectionError, AgentEventProjector, ProjectedAgentEventBatch, ProjectionState,
    pipeline_node_name,
};
use crate::agents::graph::code_trace::{CodePhaseStatus, CodeTraceEvent};

impl AgentEventProjector {
    pub(super) fn project_code_trace(
        &self,
        event: &Event,
        trace: &CodeTraceEvent,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Active(_) | ProjectionState::Complete(_)
        ) || pipeline_node_name(event)? != Some(trace.lifecycle.node_id.as_str())
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let metadata = json!({
            "langgraph_node": trace.lifecycle.node_id,
            "original_name": trace.lifecycle.node_id,
            "node_type": "code", "language": trace.lifecycle.language,
            "code_lifecycle_v1": trace.lifecycle,
        });
        let started = trace
            .started_at
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        let finished = event
            .timestamp
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        let terminal = trace.lifecycle.status != CodePhaseStatus::Started;
        let failed = trace.lifecycle.status == CodePhaseStatus::Failed;
        let id = trace.lifecycle.run_id();
        let name = format!(
            "{} / {}",
            trace.lifecycle.node_id,
            trace.lifecycle.phase.name()
        );
        let entry = json!({
            "tool_name": name, "tool_run_id": id, "run_id": id,
            "tool_meta": {"name": name, "metadata": metadata},
            "metadata": metadata, "tool_inputs": {}, "tool_output": null,
            "timestamp_start": started,
            "timestamp_finish": if terminal { Some(&finished) } else { None },
            "finish_reason": if failed { Some("error") } else if terminal { Some("stop") } else { None },
            "error": if failed { Some("The Code phase could not be completed.") } else { None },
        });
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            if terminal {
                "agent_tool_end"
            } else {
                "agent_tool_start"
            },
            &Value::Null,
            None,
            &entry,
            event.timestamp,
        )?)?;
        batch.push(self.tool_partial_event(&id, &entry, event.timestamp)?)?;
        Ok(batch)
    }
}

#[cfg(test)]
#[path = "events_code_trace_tests.rs"]
mod tests;
