//! The `agent_subagent` frames of a cloud run's sub-agent lane (#1823).

use chrono::{DateTime, Utc};
use serde_json::{Value, json};

use super::{AgentEventProjectionError, AgentEventProjector, ProjectedAgentEventBatch};
use crate::agents::subagent_lane::{SUBAGENT_EVENT, SUBAGENT_METADATA_KEY};

impl AgentEventProjector {
    /// One `agent_subagent` frame per lane entry, in order. Each carries the
    /// entry under `response_metadata.subagent_v1`; the entries are already
    /// bounded and masked (`agents::subagent_lane`).
    pub(crate) fn project_subagent_entries(
        &self,
        entries: Vec<Value>,
        occurred_at: DateTime<Utc>,
    ) -> Result<ProjectedAgentEventBatch, AgentEventProjectionError> {
        let mut batch = ProjectedAgentEventBatch::new();
        for entry in entries {
            batch.push(self.event(
                SUBAGENT_EVENT,
                &Value::Null,
                None,
                &json!({ SUBAGENT_METADATA_KEY: entry }),
                occurred_at,
            )?)?;
        }
        Ok(batch)
    }
}
