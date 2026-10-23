//! The runtime's sub-agents on a workspace turn (#1226): the desktop's
//! [`SubAgentHost`].
//!
//! Every workspace turn is offered the four tools, plan mode included (a
//! child only reads). A child calls `/llm` through the turn's own
//! [`GatewayTransport`], bound quiet (its text does not stream into the
//! parent's answer), so it carries the turn's `X-Elitea-Execution-Id` and the
//! gateway's budget gate covers its spend. It is offered the turn's local and
//! index tools (the runtime keeps only the read-only ones; the remote
//! toolkits are not offered at all), each wrapped with a [`ToolObserver`]
//! that records its calls in the commit's `tool_calls` under the
//! `spawn_agent` call (`parent_agent_call_id`, `parent_agent_path`,
//! `sibling_ordinal`: the hierarchy the cloud worker writes for nested
//! agents) and sends them to the webview as `subagent_event`s.
//!
//! Children share the session's sandbox, `path_deny`, deny list, policy and
//! prompt: an approval a child's read needs is the parent's prompt. They
//! stop with the turn: the turn closes them when its run ends, and a cancel
//! drops the run (and with it every child's task).

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_core::{Event, Llm, Part, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, ModelRequest, ModelTransport};
use elitea_agent_runtime::subagents::{ChildIdentity, ChildReport, MAX_CHILD_STEPS, SubAgentHost};
use elitea_agent_runtime::text::clip_bytes;
use serde_json::{Map, Value, json};

use super::events::TurnEvents;
use super::model::GatewayTransport;
use super::recorder::Recorder;
use super::tools::{ObservedToolset, ToolObserver};

/// The most `subagent_event`s one child sends: past it the lane shows the
/// child's end only (its calls are still recorded for the commit).
pub const MAX_EVENTS_PER_CHILD: usize = 200;
/// A child's text in one `subagent_event`, in bytes.
pub const MAX_EVENT_TEXT_BYTES: usize = 2 * 1024;

/// Where a child's tool calls go: its id for the webview, the hierarchy
/// fields for the commit.
pub struct ChildLineage {
    pub agent_id: String,
    hierarchy: Map<String, Value>,
    sent: AtomicUsize,
}

impl ChildLineage {
    #[must_use]
    pub fn new(child: &ChildIdentity) -> Self {
        // The name the transcript shows the child by: its label, else its id.
        let name = child
            .label
            .clone()
            .unwrap_or_else(|| child.agent_id.clone());
        let mut hierarchy = Map::new();
        hierarchy.insert("parent_agent_name".into(), json!(name));
        hierarchy.insert("parent_agent_call_id".into(), json!(child.parent_call_id));
        hierarchy.insert(
            "parent_agent_path".into(),
            json!([{
                "name": name,
                "call_id": child.parent_call_id,
                "sibling_ordinal": child.ordinal,
            }]),
        );
        hierarchy.insert("sibling_ordinal".into(), json!(child.ordinal));
        Self {
            agent_id: child.agent_id.clone(),
            hierarchy,
            sent: AtomicUsize::new(0),
        }
    }

    /// The fields a child's `tool_calls` entry carries.
    #[must_use]
    pub fn hierarchy(&self) -> &Map<String, Value> {
        &self.hierarchy
    }

    /// One `subagent_event`, unless the child sent its share.
    pub fn send(&self, events: &TurnEvents, inner: Value) {
        if self.sent.fetch_add(1, Ordering::SeqCst) < MAX_EVENTS_PER_CHILD {
            events.send(
                "subagent_event",
                json!({ "agent_id": self.agent_id, "inner": inner }),
            );
        }
    }
}

/// The desktop's [`SubAgentHost`] for one turn.
pub struct DesktopSubAgents {
    /// The turn's transport; children bind it quiet.
    pub transport: GatewayTransport,
    /// The parent's model request: a child keeps its model, project and
    /// generation controls, with its own system prompt.
    pub model: ModelRequest,
    /// The turn's local and index toolsets (never the remote toolkits).
    pub toolsets: Vec<Arc<dyn Toolset>>,
    pub events: Arc<TurnEvents>,
    pub recorder: Arc<Recorder>,
    /// One per child, by id: its calls' lineage.
    lineages: std::sync::Mutex<Vec<Arc<ChildLineage>>>,
}

impl DesktopSubAgents {
    #[must_use]
    pub fn new(
        transport: GatewayTransport,
        model: ModelRequest,
        toolsets: Vec<Arc<dyn Toolset>>,
        events: Arc<TurnEvents>,
        recorder: Arc<Recorder>,
    ) -> Self {
        Self {
            transport,
            model,
            toolsets,
            events,
            recorder,
            lineages: std::sync::Mutex::new(Vec::new()),
        }
    }

    fn lineage(&self, child: &ChildIdentity) -> Arc<ChildLineage> {
        let mut lineages = self
            .lineages
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(known) = lineages.iter().find(|l| l.agent_id == child.agent_id) {
            return known.clone();
        }
        let lineage = Arc::new(ChildLineage::new(child));
        lineages.push(lineage.clone());
        lineage
    }
}

#[async_trait]
impl SubAgentHost for DesktopSubAgents {
    fn bind_model(
        &self,
        _child: &ChildIdentity,
        system_instruction: &str,
    ) -> Result<Arc<dyn Llm>, HostError> {
        let transport = GatewayTransport {
            quiet: true,
            recorder: None,
            ..self.transport.clone()
        };
        Ok(transport
            .bind(ModelRequest {
                system_instruction: system_instruction.to_owned(),
                max_model_turns: MAX_CHILD_STEPS,
                ..self.model.clone()
            })?
            .adk_model())
    }

    async fn toolsets(&self, child: &ChildIdentity) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        let observer = ToolObserver {
            events: self.events.clone(),
            recorder: self.recorder.clone(),
            session: None,
            planner: None,
            child: Some(self.lineage(child)),
        };
        Ok(self
            .toolsets
            .iter()
            .map(|toolset| {
                Arc::new(ObservedToolset::new(
                    toolset.clone(),
                    observer.clone(),
                    false,
                )) as Arc<dyn Toolset>
            })
            .collect())
    }

    fn started(&self, child: &ChildIdentity) {
        let mut payload = json!({
            "agent_id": child.agent_id,
            "parent_call_id": child.parent_call_id,
            "ordinal": child.ordinal,
        });
        if let Some(label) = &child.label {
            payload["label"] = json!(label);
        }
        self.events.send("subagent_started", payload);
    }

    fn event(&self, child: &ChildIdentity, event: &Event) {
        // Its tool calls are sent by their observer; here only its text.
        if event.llm_response.partial {
            return;
        }
        let Some(content) = &event.llm_response.content else {
            return;
        };
        if content.role != "model" {
            return;
        }
        let text: String = content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect();
        if text.trim().is_empty() {
            return;
        }
        self.lineage(child).send(
            &self.events,
            json!({ "kind": "text_delta", "text": clip_bytes(&text, MAX_EVENT_TEXT_BYTES) }),
        );
    }

    fn finished(&self, child: &ChildIdentity, report: &ChildReport) {
        let mut payload = json!({
            "agent_id": child.agent_id,
            "status": report.status.as_str(),
            "steps": report.steps,
            "input_tokens": report.input_tokens,
            "output_tokens": report.output_tokens,
        });
        if let Some(result) = &report.result {
            payload["result"] = json!(result);
        }
        if let Some(error) = report.error {
            payload["error"] = json!(error);
        }
        self.events.send("subagent_finished", payload);
    }
}
