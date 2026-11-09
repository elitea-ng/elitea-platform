//! The `propose_agent` tool (ADR-0050 decision 5a, #1615), over the
//! claim-bound content listener.
//!
//! An agent whose version sets `meta.capabilities.can_propose_agents`, and
//! that may spawn sub-agents on this turn, can propose that a temporary
//! sub-agent it started become a saved agent. The proposal names the spawn
//! by its id; this side looks up that spawn's profile (soul and tools
//! included) in the execution's sub-agent ledger, so the model never
//! restates it, and sends it with the rationale, suggested instructions and
//! memory seeds.
//!
//! Main decides everything that authorizes a proposal: the project, the
//! proposing agent, the person and the conversation come from the claimed
//! execution; the capability is read from the frozen version again; the
//! person's `models.applications.applications.create` is resolved at the
//! call; the limits (three open per agent, thirty days after a dismissal)
//! are Main's. Nothing is saved as an agent: a person reviews the proposal
//! and presses Create.
//!
//! Every failure is a tool RESULT the model reads, never a failed turn.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::HostErrorCode;
use elitea_agent_runtime::subagents::Profile;
use serde_json::{Value, json};

use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::protocol::ephemeral_agents::EphemeralAgentLedger;
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::host_error;
use crate::transport::runtime_context::{
    ProposeAgentProfile, ProposeAgentRequest, ProposeAgentSeed, ProposeAgentTool,
};

pub(crate) const TOOLSET_NAME: &str = "agent_proposals";
pub(crate) const PROPOSE_AGENT: &str = "propose_agent";
/// The version meta key and capability that bind the tool.
pub(crate) const CAPABILITIES_KEY: &str = "capabilities";
pub(crate) const CAPABILITY_KEY: &str = "can_propose_agents";

const MAX_RATIONALE_BYTES: usize = 4 * 1_024;
const MAX_INSTRUCTIONS_BYTES: usize = 16 * 1_024;
const MAX_SEEDS: usize = 20;
const MAX_SEED_BYTES: usize = 2 * 1_024;
const SEED_KINDS: [&str; 3] = ["fact", "preference", "episode"];

const DESCRIPTION: &str = "Propose that a temporary sub-agent you started with spawn_agent this turn become a saved agent of this project. Use it only when the sub-agent did work people are likely to need again. Name it by the id spawn_agent answered; its profile (name, soul, tools) is sent as it was. Say why in the rationale and give the instructions the saved agent should have. You may suggest memory entries for it. A person reviews the proposal and decides; nothing is created until they press Create, and the new agent starts at the lowest risk tier. At most three of your proposals can be open in a project, and a dismissed one cannot be proposed again for 30 days.";

/// The toolset, or `None` unless the version grants the capability.
/// `toolkit_kinds` maps a bound toolkit's name to its type, so a profile
/// tool keyed by name carries its type to the review form.
#[must_use]
pub(crate) fn toolset(
    can_propose: bool,
    ledger: Arc<EphemeralAgentLedger>,
    toolkit_kinds: BTreeMap<String, String>,
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
) -> Option<Arc<dyn Toolset>> {
    if !can_propose {
        return None;
    }
    let tool: Arc<dyn Tool> = Arc::new(ProposeAgent {
        ledger,
        toolkit_kinds: toolkit_kinds
            .into_iter()
            .map(|(name, kind)| (name.trim().to_lowercase(), kind))
            .collect(),
        platform,
        authority,
    });
    Some(Arc::new(BasicToolset::new(TOOLSET_NAME, vec![tool])))
}

fn error(code: &str, message: &str) -> Value {
    json!({"status": code, "message": message})
}

/// The arguments, checked; the spawn's profile is looked up after.
struct Arguments {
    ephemeral_agent_id: String,
    rationale: String,
    instructions: String,
    memory_seeds: Vec<ProposeAgentSeed>,
}

fn valid_agent_id(id: &str) -> bool {
    id.len() == 10
        && id.starts_with("a_")
        && id[2..]
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn validate(arguments: &Value) -> Result<Arguments, Value> {
    let invalid = |message: &str| error("invalid_arguments", message);
    let Some(object) = arguments.as_object() else {
        return Err(invalid("The arguments must be an object."));
    };
    if object.keys().any(|key| {
        !matches!(
            key.as_str(),
            "ephemeral_agent_id" | "rationale" | "instructions" | "memory_seeds"
        )
    }) {
        return Err(invalid(
            "Only ephemeral_agent_id, rationale, instructions and memory_seeds are accepted.",
        ));
    }
    let text = |key: &str| {
        object
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or_default()
            .trim()
            .to_owned()
    };
    let ephemeral_agent_id = text("ephemeral_agent_id");
    if !valid_agent_id(&ephemeral_agent_id) {
        return Err(invalid(
            "ephemeral_agent_id must be the id spawn_agent answered (a_ and 8 hex digits).",
        ));
    }
    let rationale = text("rationale");
    if rationale.is_empty() || rationale.len() > MAX_RATIONALE_BYTES {
        return Err(invalid(
            "rationale must say why the sub-agent should be saved, in at most 4096 bytes.",
        ));
    }
    let instructions = text("instructions");
    if instructions.len() > MAX_INSTRUCTIONS_BYTES {
        return Err(invalid("instructions are at most 16384 bytes."));
    }
    let seeds = match object.get("memory_seeds") {
        None | Some(Value::Null) => &[][..],
        Some(Value::Array(items)) if items.len() <= MAX_SEEDS => items.as_slice(),
        Some(_) => {
            return Err(invalid("memory_seeds is a list of at most 20 entries."));
        }
    };
    let mut memory_seeds = Vec::with_capacity(seeds.len());
    for seed in seeds {
        let kind = seed
            .get("kind")
            .and_then(Value::as_str)
            .map_or("fact", str::trim);
        let content = seed
            .get("content")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .trim();
        if !SEED_KINDS.contains(&kind) || content.is_empty() || content.len() > MAX_SEED_BYTES {
            return Err(invalid(
                "each memory seed is {kind: fact, preference or episode; content: at most 2048 bytes}.",
            ));
        }
        memory_seeds.push(ProposeAgentSeed {
            kind: kind.to_owned(),
            content: content.to_owned(),
        });
    }
    Ok(Arguments {
        ephemeral_agent_id,
        rationale,
        instructions,
        memory_seeds,
    })
}

/// The spawn's profile as Main reads it: the tool keys canonical, each with
/// its toolkit's type when the toolkit half names a bound toolkit.
fn reported_profile(
    label: String,
    profile: &Profile,
    toolkit_kinds: &BTreeMap<String, String>,
) -> ProposeAgentProfile {
    ProposeAgentProfile {
        label,
        display_name: profile.display_name.clone().unwrap_or_default(),
        soul: profile.soul.clone().unwrap_or_default(),
        avatar: profile.avatar.clone().unwrap_or_default(),
        tools: profile.tools.as_ref().map(|tools| {
            tools
                .iter()
                .map(|tool| {
                    let key = tool.canonical();
                    let toolkit_type = key
                        .split_once('/')
                        .and_then(|(toolkit, _)| toolkit_kinds.get(toolkit))
                        .cloned()
                        .unwrap_or_default();
                    ProposeAgentTool { key, toolkit_type }
                })
                .collect()
        }),
    }
}

struct ProposeAgent {
    ledger: Arc<EphemeralAgentLedger>,
    toolkit_kinds: BTreeMap<String, String>,
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

#[async_trait]
impl Tool for ProposeAgent {
    fn name(&self) -> &str {
        PROPOSE_AGENT
    }

    fn description(&self) -> &str {
        DESCRIPTION
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
                "ephemeral_agent_id": {
                    "type": "string",
                    "description": "The id spawn_agent answered for the sub-agent, a_ and 8 hex digits."
                },
                "rationale": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": MAX_RATIONALE_BYTES,
                    "description": "Why the sub-agent should be saved: what it did well and who would use it."
                },
                "instructions": {
                    "type": "string",
                    "maxLength": MAX_INSTRUCTIONS_BYTES,
                    "description": "The instructions the saved agent should have."
                },
                "memory_seeds": {
                    "type": "array",
                    "maxItems": MAX_SEEDS,
                    "items": {
                        "type": "object",
                        "properties": {
                            "kind": {"type": "string", "enum": SEED_KINDS},
                            "content": {"type": "string", "minLength": 1, "maxLength": MAX_SEED_BYTES}
                        },
                        "required": ["content"],
                        "additionalProperties": false
                    },
                    "description": "Memory entries the saved agent could start with; a person picks the ones to keep."
                }
            },
            "required": ["ephemeral_agent_id", "rationale"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let arguments = match validate(&arguments) {
            Ok(arguments) => arguments,
            Err(result) => return Ok(result),
        };
        let Some((label, profile)) = self.ledger.profile(&arguments.ephemeral_agent_id) else {
            return Ok(error(
                "ephemeral_agent_unknown",
                "No sub-agent with this id was started in this run. Propose only a sub-agent you spawned this turn.",
            ));
        };
        let request = ProposeAgentRequest {
            profile: reported_profile(label, &profile, &self.toolkit_kinds),
            ephemeral_agent_id: arguments.ephemeral_agent_id,
            rationale: arguments.rationale,
            instructions: arguments.instructions,
            memory_seeds: arguments.memory_seeds,
        };
        match self.platform.propose_agent(&self.authority, &request).await {
            Ok(response) => Ok(response),
            Err(transport) => {
                let failure = host_error(&transport);
                tracing::warn!(
                    event = "agent_internal_tool_failed",
                    internal_tool = PROPOSE_AGENT,
                    reason_code = failure.reason_code(),
                    "the propose_agent tool call failed"
                );
                Ok(match failure.code() {
                    HostErrorCode::InvalidInput | HostErrorCode::ResourceExhausted => error(
                        "rejected",
                        "The platform refused this proposal; do not retry it as written.",
                    ),
                    _ => error(
                        "proposals_unavailable",
                        "The proposal could not be made right now. Tell the person instead.",
                    ),
                })
            }
        }
    }
}

#[cfg(test)]
#[path = "propose_agent_tool_tests.rs"]
mod tests;
