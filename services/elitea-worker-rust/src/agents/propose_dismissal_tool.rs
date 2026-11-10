//! The `propose_dismissal` tool (ADR-0050 decision 5l, #1616), over the
//! claim-bound content listener.
//!
//! An HR agent (its version sets `meta.capabilities.can_review_agents`) may
//! propose a ladder stage (watch, demote with an exact narrowing, suspend or
//! retire) for another agent of its project, with a rationale. It never
//! decides: Main stores a proposal and a person who manages agents approves
//! or rejects it.
//!
//! Main decides everything that authorizes a proposal: the project, the
//! proposing agent and the person come from the claimed execution; the
//! capability is read from the frozen version again; the guards (not
//! itself, not its hirer, not a higher tier, not another HR agent, not a
//! critical-tier agent or one under legal hold, one open proposal per
//! agent) are Main's.
//!
//! Every failure is a tool RESULT the model reads, never a failed turn.

use std::sync::Arc;

use adk_rust::tool::BasicToolset;
use adk_rust::{Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::HostErrorCode;
use serde_json::{Map, Value, json};

use crate::protocol::control::ClaimBoundRuntimeContextAuthority;
use crate::transport::platform_client::PlatformClient;
use crate::transport::platform_writer::host_error;
use crate::transport::runtime_context::{DismissalNarrowing, ProposeDismissalRequest};

pub(crate) const TOOLSET_NAME: &str = "agent_workforce";
pub(crate) const PROPOSE_DISMISSAL: &str = "propose_dismissal";
/// The capability that binds the tool, under `meta.capabilities`.
pub(crate) const CAPABILITY_KEY: &str = "can_review_agents";

const STAGES: [&str; 4] = ["watch", "demote", "suspend", "retire"];
const CAPABILITIES: [&str; 2] = ["can_propose_agents", "can_review_agents"];
const MAX_RATIONALE_BYTES: usize = 4 * 1_024;
const MAX_LIST: usize = 50;
const MAX_TYPE_BYTES: usize = 128;

const DESCRIPTION: &str = "Propose a workforce stage for another agent of this project, as its reviewer: watch (flag it for review), demote (narrow it: name exactly what to take away), suspend, or retire. Base it on evidence (its scorecard, failures, spend, feedback) and say what in the rationale. You never decide: a person who manages agents approves or rejects the proposal. You cannot propose a stage for yourself, the agent that hired you, an agent of a higher risk tier, another reviewer agent, a critical-tier agent or one under legal hold, and only one proposal per agent can be open.";

/// The toolset, or `None` unless the version grants the capability.
#[must_use]
pub(crate) fn toolset(
    can_review: bool,
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
) -> Option<Arc<dyn Toolset>> {
    if !can_review {
        return None;
    }
    let tool: Arc<dyn Tool> = Arc::new(ProposeDismissal {
        platform,
        authority,
    });
    Some(Arc::new(BasicToolset::new(TOOLSET_NAME, vec![tool])))
}

fn error(code: &str, message: &str) -> Value {
    json!({"status": code, "message": message})
}

fn id_list(object: &Map<String, Value>, key: &str) -> Result<Vec<i64>, ()> {
    match object.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) if items.len() <= MAX_LIST => items
            .iter()
            .map(|item| item.as_i64().filter(|id| *id > 0).ok_or(()))
            .collect(),
        Some(_) => Err(()),
    }
}

fn text_list(
    object: &Map<String, Value>,
    key: &str,
    allowed: Option<&[&str]>,
) -> Result<Vec<String>, ()> {
    match object.get(key) {
        None | Some(Value::Null) => Ok(Vec::new()),
        Some(Value::Array(items)) if items.len() <= MAX_LIST => items
            .iter()
            .map(|item| {
                let text = item.as_str().map(str::trim).unwrap_or_default();
                let known = allowed.is_none_or(|allowed| allowed.contains(&text));
                if text.is_empty() || text.len() > MAX_TYPE_BYTES || !known {
                    Err(())
                } else {
                    Ok(text.to_owned())
                }
            })
            .collect(),
        Some(_) => Err(()),
    }
}

fn flag(object: &Map<String, Value>, key: &str) -> Result<bool, ()> {
    match object.get(key) {
        None | Some(Value::Null) => Ok(false),
        Some(Value::Bool(value)) => Ok(*value),
        Some(_) => Err(()),
    }
}

fn narrowing(value: Option<&Value>) -> Result<DismissalNarrowing, Value> {
    let invalid = || {
        error(
            "invalid_arguments",
            "narrowing is {remove_toolkit_ids: [toolkit ids], memory_review, proactivity_off, remove_lead: booleans, remove_capabilities: [can_propose_agents, can_review_agents], toolkit_type_ceiling: [toolkit types]}.",
        )
    };
    let object = match value {
        None | Some(Value::Null) => return Ok(DismissalNarrowing::default()),
        Some(Value::Object(object)) => object,
        Some(_) => return Err(invalid()),
    };
    if object.keys().any(|key| {
        !matches!(
            key.as_str(),
            "remove_toolkit_ids"
                | "memory_review"
                | "proactivity_off"
                | "remove_capabilities"
                | "remove_lead"
                | "toolkit_type_ceiling"
        )
    }) {
        return Err(invalid());
    }
    let read = || -> Result<DismissalNarrowing, ()> {
        Ok(DismissalNarrowing {
            remove_toolkit_ids: id_list(object, "remove_toolkit_ids")?,
            memory_review: flag(object, "memory_review")?,
            proactivity_off: flag(object, "proactivity_off")?,
            remove_capabilities: text_list(object, "remove_capabilities", Some(&CAPABILITIES))?,
            remove_lead: flag(object, "remove_lead")?,
            toolkit_type_ceiling: text_list(object, "toolkit_type_ceiling", None)?,
        })
    };
    read().map_err(|()| invalid())
}

fn validate(arguments: &Value) -> Result<ProposeDismissalRequest, Value> {
    let invalid = |message: &str| error("invalid_arguments", message);
    let Some(object) = arguments.as_object() else {
        return Err(invalid("The arguments must be an object."));
    };
    if object.keys().any(|key| {
        !matches!(
            key.as_str(),
            "application_id" | "stage" | "rationale" | "narrowing"
        )
    }) {
        return Err(invalid(
            "Only application_id, stage, rationale and narrowing are accepted.",
        ));
    }
    let Some(application_id) = object
        .get("application_id")
        .and_then(Value::as_i64)
        .filter(|id| *id > 0 && *id <= i64::from(i32::MAX))
    else {
        return Err(invalid(
            "application_id must be the id of an agent of this project.",
        ));
    };
    let stage = object
        .get("stage")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if !STAGES.contains(&stage) {
        return Err(invalid("stage is watch, demote, suspend or retire."));
    }
    let rationale = object
        .get("rationale")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim();
    if rationale.is_empty() || rationale.len() > MAX_RATIONALE_BYTES {
        return Err(invalid(
            "rationale must give the evidence for the stage, in at most 4096 bytes.",
        ));
    }
    let narrowing = narrowing(object.get("narrowing"))?;
    if stage == "demote" && narrowing.is_empty() {
        return Err(invalid(
            "A demotion names exactly what to take away in narrowing.",
        ));
    }
    if stage != "demote" && !narrowing.is_empty() {
        return Err(invalid("Only a demotion takes a narrowing."));
    }
    Ok(ProposeDismissalRequest {
        application_id,
        stage: stage.to_owned(),
        rationale: rationale.to_owned(),
        narrowing,
    })
}

struct ProposeDismissal {
    platform: Arc<PlatformClient>,
    authority: Arc<ClaimBoundRuntimeContextAuthority>,
}

#[async_trait]
impl Tool for ProposeDismissal {
    fn name(&self) -> &str {
        PROPOSE_DISMISSAL
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
                "application_id": {
                    "type": "integer",
                    "minimum": 1,
                    "description": "The id of the agent the stage is for."
                },
                "stage": {
                    "type": "string",
                    "enum": STAGES,
                    "description": "watch, demote, suspend or retire."
                },
                "rationale": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": MAX_RATIONALE_BYTES,
                    "description": "The evidence for the stage: what the agent did and why this stage fits."
                },
                "narrowing": {
                    "type": "object",
                    "description": "Only for demote: exactly what to take away.",
                    "properties": {
                        "remove_toolkit_ids": {"type": "array", "maxItems": MAX_LIST, "items": {"type": "integer", "minimum": 1}},
                        "memory_review": {"type": "boolean", "description": "Its memory writes need review."},
                        "proactivity_off": {"type": "boolean", "description": "Turn its heartbeat off."},
                        "remove_capabilities": {"type": "array", "items": {"type": "string", "enum": CAPABILITIES}},
                        "remove_lead": {"type": "boolean", "description": "It stops leading conversations."},
                        "toolkit_type_ceiling": {"type": "array", "maxItems": MAX_LIST, "items": {"type": "string"}, "description": "Keep only toolkits of these types."}
                    },
                    "additionalProperties": false
                }
            },
            "required": ["application_id", "stage", "rationale"],
            "additionalProperties": false
        }))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let request = match validate(&arguments) {
            Ok(request) => request,
            Err(result) => return Ok(result),
        };
        match self
            .platform
            .propose_dismissal(&self.authority, &request)
            .await
        {
            Ok(response) => Ok(response),
            Err(transport) => {
                let failure = host_error(&transport);
                tracing::warn!(
                    event = "agent_internal_tool_failed",
                    internal_tool = PROPOSE_DISMISSAL,
                    reason_code = failure.reason_code(),
                    "the propose_dismissal tool call failed"
                );
                Ok(match failure.code() {
                    HostErrorCode::InvalidInput | HostErrorCode::ResourceExhausted => error(
                        "rejected",
                        "The platform refused this proposal; do not retry it as written.",
                    ),
                    _ => error(
                        "workforce_unavailable",
                        "The proposal could not be made right now. Tell the person instead.",
                    ),
                })
            }
        }
    }
}

#[cfg(test)]
#[path = "propose_dismissal_tool_tests.rs"]
mod tests;
