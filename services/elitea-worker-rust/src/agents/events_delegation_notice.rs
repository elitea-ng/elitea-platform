//! The `delegation_refused` and `delegation_narrowed` frames of a delegation
//! the run refused or narrowed (ADR-0044 Amendment 1, A1.2 rule 5, #2161).
//!
//! The model keeps the typed refusal as the tool result. This is the same
//! fact for the person: one frame next to the callee's tool-call row, so the
//! chat shows it at the sub-agent's place instead of a failure only the model
//! reads.

use chrono::{DateTime, Utc};
use elitea_agent_runtime::delegation::{NOTICE_METADATA_KEY, NoticeContext};
use serde_json::{Value, json};

use super::{ActiveToolCall, AgentEventProjectionError, AgentEventProjector, NodeEventV1};

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
