//! A debug snapshot uses an inert receipt. It never becomes a model tool result.
use super::{
    AgentEventProjectionError, AgentEventProjector, ProjectedAgentEventBatch, ProjectionState,
    pipeline_node_name,
};
use crate::agents::graph::code_debug::CodeDebugProof;
use adk_rust::Event;
use chrono::SecondsFormat;
use serde_json::{Value, json};
impl AgentEventProjector {
    pub(super) fn project_code_debug(
        &self,
        event: &Event,
        proof: &CodeDebugProof,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        if !matches!(
            self.state,
            ProjectionState::Started | ProjectionState::Active(_) | ProjectionState::Complete(_)
        ) || pipeline_node_name(event)? != Some(proof.node_id.as_str())
        {
            return Err(AgentEventProjectionError::invalid_state());
        }
        let metadata = json!({"langgraph_node": proof.node_id, "original_name": proof.node_id,
            "node_type": "code", "code_debug_v1": proof});
        let time = event
            .timestamp
            .to_rfc3339_opts(SecondsFormat::AutoSi, false);
        let id = proof.run_id();
        let name = format!("{} / debug export", proof.node_id);
        let entry = json!({"tool_name": name, "tool_run_id": id, "run_id": id,
            "tool_meta": {"name": name, "metadata": metadata}, "metadata": metadata,
            "tool_inputs": {}, "tool_output": null, "timestamp_start": time, "timestamp_finish": time,
            "finish_reason": "stop", "error": null});
        let mut batch = ProjectedAgentEventBatch::new();
        batch.push(self.event(
            "agent_tool_end",
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
mod tests {
    use super::*;
    use crate::agents::{
        events::AgentEventProjectionContext,
        graph::{PIPELINE_NODE_METADATA_KEY, code_debug::CODE_DEBUG_METADATA_KEY},
    };
    fn event(status: &str) -> Event {
        let proof = CodeDebugProof {
            revision: 1,
            original_visit: crate::sandbox::code_recovery::OriginalCodeVisitRef {
                visit_id: "1".repeat(64),
                revision: 1,
                digest_sha256: "2".repeat(64),
            },
            attempt: 1,
            execution_id: "execution-1".into(),
            generation: "7".into(),
            node_id: "Code_1".into(),
            activation_id: "a".repeat(64),
            request_sha256: "b".repeat(64),
            status: status.into(),
            artifact: None,
        };
        let mut e = Event::with_id("debug-event", "invocation-1");
        e.author = "root-agent".into();
        e.llm_response.partial = true;
        e.provider_metadata.insert(
            CODE_DEBUG_METADATA_KEY.into(),
            serde_json::to_string(&proof).unwrap(),
        );
        e.provider_metadata
            .insert(PIPELINE_NODE_METADATA_KEY.into(), "Code_1".into());
        e
    }
    fn projector() -> AgentEventProjector {
        let mut p =
            AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({}))).unwrap();
        p.start(chrono::Utc::now()).unwrap();
        p
    }
    #[test]
    fn warning_trace_is_complete_data_free_and_has_no_model_turn() {
        for status in ["denied", "unavailable"] {
            let mut p = projector();
            let batch: Vec<_> = p.project(&event(status)).unwrap().into_iter().collect();
            assert_eq!(batch.len(), 2);
            assert_eq!(batch[0].r#type, "agent_tool_end");
            assert_eq!(batch[1].r#type, "partial_message");
            let metadata: Value = serde_json::from_slice(&batch[1].response_metadata).unwrap();
            let entries = metadata["tool_calls"].as_object().unwrap();
            let entry = entries.values().next().unwrap();
            assert_eq!(entry["tool_inputs"], json!({}));
            assert_eq!(entry["tool_output"], Value::Null);
            assert_eq!(entry["metadata"]["code_debug_v1"]["status"], status);
            assert!(entry["metadata"]["code_debug_v1"].get("artifact").is_none());
            assert_eq!(entry["timestamp_start"], entry["timestamp_finish"]);
            assert!(p.active_tools.is_empty());
            assert!(matches!(p.state, ProjectionState::Started));
        }
    }
    #[test]
    fn payload_or_conflicting_node_is_rejected_before_projection() {
        for kind in ["node", "source", "state"] {
            let mut e = event("unavailable");
            match kind {
                "node" => {
                    e.provider_metadata
                        .insert(PIPELINE_NODE_METADATA_KEY.into(), "other".into());
                }
                "source" => {
                    let mut v: Value =
                        serde_json::from_str(&e.provider_metadata[CODE_DEBUG_METADATA_KEY])
                            .unwrap();
                    v["source"] = json!("private");
                    e.provider_metadata
                        .insert(CODE_DEBUG_METADATA_KEY.into(), v.to_string());
                }
                _ => {
                    e.actions
                        .state_delta
                        .insert("private".into(), json!("state"));
                }
            }
            assert!(projector().project(&e).is_err());
        }
    }
}
