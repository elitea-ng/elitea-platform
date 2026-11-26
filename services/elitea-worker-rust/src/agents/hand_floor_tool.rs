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
use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::hand_floor;
use elitea_agent_runtime::host::HostErrorCode;
use serde_json::Value;

use super::request::{AgentFloor, AgentFloorPeer};
use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::host_error;
use crate::transport::runtime_context::HandFloorRequest;

pub(crate) const TOOLSET_NAME: &str = hand_floor::TOOLSET_NAME;
pub(crate) const HAND_FLOOR: &str = hand_floor::HAND_FLOOR;

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

/// The description the desktop host gives the same peers (ADR-0061
/// decision 3d): both call the shared function.
fn describe(peers: &[AgentFloorPeer]) -> String {
    hand_floor::describe(
        &peers
            .iter()
            .map(|peer| hand_floor::Peer {
                participant_id: peer.participant_id,
                name: peer.name.clone(),
                role: peer.role.clone(),
            })
            .collect::<Vec<_>>(),
    )
}

fn error(code: &str, message: &str) -> Value {
    hand_floor::error(code, message)
}

/// The arguments, checked against this turn's peers (the shared check).
fn validate(arguments: &Value, peers: &HashSet<i64>) -> Result<HandFloorRequest, Value> {
    hand_floor::validate(arguments, peers).map(|args| HandFloorRequest {
        participant_ids: args.participant_ids,
        message: args.message,
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
        Some(hand_floor::parameters_schema())
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
