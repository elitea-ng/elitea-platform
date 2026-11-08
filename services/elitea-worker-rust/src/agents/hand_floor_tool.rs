//! The turn-taking `hand_floor` tool (ADR-0050 decision 11, #1611), over
//! the claim-bound content listener.
//!
//! An agent that holds the floor in a conversation with other agents can
//! hand it to one or more of them: they answer after this turn ends, in
//! order. Main decides everything that authorizes a hand-off: who is in the
//! conversation, the order, and when the loop guard stops the exchange. This
//! side checks the arguments against the peers Main named for this turn, so
//! a malformed call never leaves the worker, and passes Main's answer to the
//! model as written.
//!
//! Every failure is a tool RESULT the model reads, never a failed turn.

use std::collections::HashSet;
use std::fmt::Write as _;
use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::HostErrorCode;
use elitea_agent_runtime::toolkits::FrozenToolset;
use serde_json::{Value, json};

use super::request::{AgentFloor, AgentFloorPeer};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::host_error;
use crate::transport::runtime_context::HandFloorRequest;

pub(crate) const TOOLSET_NAME: &str = "turn_taking";
pub(crate) const HAND_FLOOR: &str = "hand_floor";
/// The most agents one hand-off names.
const MAX_HAND_OFF_PEERS: usize = 4;
/// The longest hand-off message, in bytes.
const MAX_MESSAGE_BYTES: usize = 4_000;

/// The toolset for `floor`, or `None` when there is nobody to hand it to.
#[must_use]
pub(crate) fn toolset(
    floor: &AgentFloor,
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
) -> Option<Arc<dyn Toolset>> {
    if floor.peers.is_empty() {
        return None;
    }
    let tool: Arc<dyn Tool> = Arc::new(HandFloorTool {
        description: describe(&floor.peers),
        peers: floor.peers.iter().map(|peer| peer.participant_id).collect(),
        platform,
        authority,
    });
    Some(Arc::new(BasicToolset::new(TOOLSET_NAME, vec![tool])))
}

/// Keep only the read-only tools of `toolsets` (an unattended floor turn,
/// ADR-0050 decision 11), with how many tools were kept and dropped. A
/// toolset left with no tools is dropped whole.
#[must_use]
pub(crate) fn retain_read_only(toolsets: Vec<FrozenToolset>) -> (Vec<FrozenToolset>, usize, usize) {
    let (mut kept, mut dropped) = (0, 0);
    let mut retained = Vec::with_capacity(toolsets.len());
    for toolset in toolsets {
        let tools: Vec<Arc<dyn Tool>> = toolset
            .tools()
            .iter()
            .filter(|tool| tool.is_read_only())
            .cloned()
            .collect();
        kept += tools.len();
        dropped += toolset.tools().len() - tools.len();
        if !tools.is_empty() {
            retained.push(FrozenToolset::new(toolset.name(), tools));
        }
    }
    (retained, kept, dropped)
}

fn describe(peers: &[AgentFloorPeer]) -> String {
    let mut description = String::from(
        "Hand the floor to other agents in this conversation. They answer after your turn ends, in order. Use it only when another agent is better placed to do part of the work; do not hand the floor back and forth. A loop guard stops the exchange after a few hand-offs.\n\nThe agents you can hand the floor to:",
    );
    for peer in peers {
        let _ = write!(
            description,
            "\n- {} ({}) — participant {}",
            peer.name, peer.role, peer.participant_id
        );
    }
    description
}

fn error(code: &str, message: &str) -> Value {
    json!({"status": "error", "code": code, "message": message})
}

/// The arguments, checked against this turn's peers.
fn validate(arguments: &Value, peers: &HashSet<i64>) -> Result<HandFloorRequest, Value> {
    let invalid = |message: &str| error("invalid_argument", message);
    let Some(object) = arguments.as_object() else {
        return Err(invalid("The arguments must be an object."));
    };
    if object
        .keys()
        .any(|key| key != "participant_ids" && key != "message")
    {
        return Err(invalid("Only participant_ids and message are accepted."));
    }
    let Some(ids) = object.get("participant_ids").and_then(Value::as_array) else {
        return Err(invalid(
            "participant_ids must be a list of participant ids.",
        ));
    };
    if ids.is_empty() || ids.len() > MAX_HAND_OFF_PEERS {
        return Err(invalid("participant_ids must name one to four agents."));
    }
    let mut seen = HashSet::with_capacity(ids.len());
    let mut participant_ids = Vec::with_capacity(ids.len());
    for id in ids {
        let Some(id) = id.as_i64() else {
            return Err(invalid("participant_ids must be integers."));
        };
        if !peers.contains(&id) {
            return Err(invalid(
                "Every participant id must be one of the agents listed in this tool's description.",
            ));
        }
        if !seen.insert(id) {
            return Err(invalid("participant_ids must not repeat an agent."));
        }
        participant_ids.push(id);
    }
    let message = object
        .get("message")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if message.is_empty() || message.len() > MAX_MESSAGE_BYTES {
        return Err(invalid(
            "message must say what you want them to do, in at most 4000 bytes.",
        ));
    }
    Ok(HandFloorRequest {
        participant_ids,
        message: message.to_owned(),
    })
}

struct HandFloorTool {
    description: String,
    peers: HashSet<i64>,
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

#[async_trait]
impl Tool for HandFloorTool {
    fn name(&self) -> &str {
        HAND_FLOOR
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        false
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "participant_ids": {
                    "type": "array",
                    "items": {"type": "integer"},
                    "minItems": 1,
                    "maxItems": MAX_HAND_OFF_PEERS,
                    "uniqueItems": true
                },
                "message": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": MAX_MESSAGE_BYTES,
                    "description": "What you want them to do."
                }
            },
            "required": ["participant_ids", "message"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let request = match validate(&arguments, &self.peers) {
            Ok(request) => request,
            Err(result) => return Ok(result),
        };
        match self.platform.hand_floor(&self.authority, &request).await {
            Ok(response) => Ok(response),
            Err(transport) => {
                let failure = host_error(&transport);
                tracing::warn!(
                    event = "agent_internal_tool_failed",
                    internal_tool = HAND_FLOOR,
                    reason_code = failure.reason_code(),
                    "the hand_floor tool call failed"
                );
                Ok(match failure.code() {
                    HostErrorCode::InvalidInput | HostErrorCode::ResourceExhausted => error(
                        "rejected",
                        "The conversation refused this hand-off; do not retry it as written.",
                    ),
                    _ => error(
                        "turn_taking_unavailable",
                        "The floor could not be handed right now. Finish your answer yourself.",
                    ),
                })
            }
        }
    }
}

#[cfg(test)]
#[path = "hand_floor_tool_tests.rs"]
mod tests;
