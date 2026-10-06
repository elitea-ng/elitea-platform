use super::*;
use crate::agents::events::AgentEventProjectionContext;
use crate::agents::graph::{
    PIPELINE_NODE_METADATA_KEY,
    code_trace::{CODE_TRACE_METADATA_KEY, CodeLifecycle, CodePhase, CodePhaseStatus},
};
use adk_rust::Part;
use chrono::{TimeZone, Utc};
fn event(status: CodePhaseStatus) -> Event {
    let started = Utc
        .with_ymd_and_hms(2026, 10, 2, 12, 0, 0)
        .single()
        .unwrap();
    let mut event = Event::with_id("code-phase-event", "invocation-1");
    event.author = "root-agent".into();
    event.timestamp = started + chrono::Duration::seconds(1);
    event.llm_response.partial = true;
    let trace = CodeTraceEvent {
        started_at: started,
        lifecycle: CodeLifecycle {
            revision: 1,
            execution_id: "execution-1".into(),
            generation: "7".into(),
            activation_id: "a".repeat(64),
            node_id: "Code_1".into(),
            graph_thread_id: "root/child/grandchild".into(),
            graph_step: "4".into(),
            language: "python".into(),
            phase: CodePhase::Execution,
            status,
        },
    };
    event.provider_metadata.insert(
        CODE_TRACE_METADATA_KEY.into(),
        serde_json::to_string(&trace).unwrap(),
    );
    event
        .provider_metadata
        .insert(PIPELINE_NODE_METADATA_KEY.into(), "Code_1".into());
    event
}
fn projector() -> AgentEventProjector {
    let mut projector =
        AgentEventProjector::new(AgentEventProjectionContext::fixture(json!({}))).unwrap();
    projector.start(Utc::now()).unwrap();
    projector
}
#[test]
fn code_phases_project_existing_tool_deltas_without_a_model_turn() {
    let mut projector = projector();
    for status in [
        CodePhaseStatus::Started,
        CodePhaseStatus::Completed,
        CodePhaseStatus::Failed,
    ] {
        let batch: Vec<_> = projector
            .project(&event(status))
            .unwrap()
            .into_iter()
            .collect();
        assert_eq!(batch.len(), 2);
        assert_eq!(
            batch[0].r#type,
            if status == CodePhaseStatus::Started {
                "agent_tool_start"
            } else {
                "agent_tool_end"
            }
        );
        assert_eq!(batch[1].r#type, "partial_message");
        assert_eq!(batch[1].message_id, Some("message-1".into()));
        let metadata: Value = serde_json::from_slice(&batch[1].response_metadata).unwrap();
        let entries = metadata["tool_calls"].as_object().unwrap();
        assert_eq!(entries.len(), 1);
        let entry = entries.values().next().unwrap();
        assert_eq!(entry["tool_inputs"], json!({}));
        assert_eq!(entry["tool_output"], Value::Null);
        assert_eq!(
            entry["metadata"]["code_lifecycle_v1"]["execution_id"],
            "execution-1"
        );
        assert_eq!(entry["metadata"]["code_lifecycle_v1"]["graph_step"], "4");
        assert!(projector.active_tools.is_empty());
        assert!(matches!(projector.state, ProjectionState::Started));
    }
}
#[test]
fn code_trace_rejects_node_conflict_unknown_fields_and_payloads() {
    for mutation in ["node", "unknown", "payload", "oversize"] {
        let mut event = event(CodePhaseStatus::Started);
        match mutation {
            "node" => {
                event
                    .provider_metadata
                    .insert(PIPELINE_NODE_METADATA_KEY.into(), "other-node".into());
            }
            "unknown" => {
                let mut value: Value =
                    serde_json::from_str(&event.provider_metadata[CODE_TRACE_METADATA_KEY])
                        .unwrap();
                value["lifecycle"]["source"] = json!("private-source");
                event
                    .provider_metadata
                    .insert(CODE_TRACE_METADATA_KEY.into(), value.to_string());
            }
            "payload" => {
                event.llm_response.content = Some(adk_rust::Content {
                    role: "model".into(),
                    parts: vec![Part::Text {
                        text: "private-source".into(),
                    }],
                });
            }
            "oversize" => {
                event
                    .provider_metadata
                    .insert(CODE_TRACE_METADATA_KEY.into(), "x".repeat(2049));
            }
            _ => unreachable!(),
        }
        assert!(projector().project(&event).is_err(), "mutation {mutation}");
    }
}
