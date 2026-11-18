//! The `delegation_refused` and `delegation_narrowed` frames of a delegation
//! the run refused or narrowed (ADR-0044 Amendment 1, A1.2 rule 5, #2161).
//!
//! The model keeps the typed refusal as the tool result. This is the same
//! fact for the person: one frame next to the callee's tool-call row, so the
//! chat shows it at the sub-agent's place instead of a failure only the model
//! reads.

use chrono::{DateTime, SecondsFormat, Utc};
use elitea_agent_runtime::delegation::{DelegationNotice, NOTICE_METADATA_KEY, NoticeContext};
use serde_json::{Value, json};

use super::{
    ActiveToolCall, AgentEventProjectionError, AgentEventProjector, NodeEventV1, ProtocolError,
    encode_current_node_event_json,
};

impl AgentEventProjector {
    /// The notice frame of a completed saved-agent or saved-pipeline call, or
    /// `None` when the delegation was neither refused nor narrowed.
    pub(super) fn delegation_notice_event(
        &self,
        call_id: &str,
        active: &ActiveToolCall,
        occurred_at: DateTime<Utc>,
    ) -> Result<Option<NodeEventV1>, AgentEventProjectionError> {
        let Some(application) = active.application.as_ref() else {
            return Ok(None);
        };
        let Some(evidence) = application.guards.delegation() else {
            return Ok(None);
        };
        // The root's name is the caller's only at the first hop; a nested
        // caller's notice names no caller and the clients say "its caller".
        let caller_name = (evidence.depth == 1).then_some(self.context.root_agent_name.as_str());
        let context = NoticeContext {
            call_id,
            callee_name: &application.display_name,
            caller_name,
            edge_kind: if application.agent_type == "pipeline" {
                "pipeline_child"
            } else {
                "agent_tool"
            },
        };
        let Some(notice) = evidence.notice(&context) else {
            return Ok(None);
        };
        self.event(
            notice.event,
            &Value::Null,
            None,
            &json!({ NOTICE_METADATA_KEY: notice.value }),
            occurred_at,
        )
        .map(Some)
    }
}

/// The notice frame of a delegation refused while the run was being
/// assembled: a pipeline node that runs a saved pipeline its caller may not
/// delegate to (#2218). No tool call carries it, so the lifecycle publishes
/// it before the terminal failure, stamped with the identity the verified
/// command binds (the projector cannot be built when assembly failed).
pub(crate) fn assembly_delegation_notice(
    identity: &super::AssemblyNoticeIdentity<'_>,
    notice: &DelegationNotice,
    occurred_at: DateTime<Utc>,
) -> Result<NodeEventV1, AgentEventProjectionError> {
    let fields = [
        identity.stream_id,
        identity.message_id,
        identity.sio_event,
        identity.execution_generation,
    ];
    if fields.iter().any(|value| {
        value.is_empty()
            || value.len() > super::MAX_CONTEXT_TEXT_BYTES
            || value
                .bytes()
                .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
    }) {
        return Err(AgentEventProjectionError::invalid_state());
    }
    let output = |_: serde_json::Error| {
        AgentEventProjectionError::output(ProtocolError::InvalidInput(
            "the delegation notice is malformed",
        ))
    };
    let event = NodeEventV1 {
        r#type: notice.event.to_owned(),
        stream_id: Some(identity.stream_id.to_owned()),
        message_id: Some(identity.message_id.to_owned()),
        question_id: None,
        content: serde_json::to_vec(&Value::Null).map_err(output)?,
        thinking: None,
        response_metadata: serde_json::to_vec(&json!({ NOTICE_METADATA_KEY: notice.value }))
            .map_err(output)?,
        references: b"[]".to_vec(),
        sio_event: Some(identity.sio_event.to_owned()),
        created_at: Some(occurred_at.to_rfc3339_opts(SecondsFormat::AutoSi, false)),
        parent_message_id: None,
        agent_name: None,
        execution_generation: Some(identity.execution_generation.to_owned()),
    };
    encode_current_node_event_json(&event).map_err(AgentEventProjectionError::output)?;
    Ok(event)
}
