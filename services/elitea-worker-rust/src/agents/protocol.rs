use std::collections::HashSet;

use prost::Message;
use serde_json::{Map, Value};

use super::request::{
    AgentExecutionKind, AgentExecutionPayload, AgentExecutionRequest, AgentInputBinding,
    MAX_AGENT_INSTRUCTION_BYTES, MAX_AGENT_USER_INPUT_BYTES, NextInputSuggestionPolicy, UserInput,
};
use super::unattended::{UnattendedGrant, grant_from_input};
use crate::protocol::{
    InputLimitField, ProtocolError as AgentProtocolError,
    elitea::runtime::v1::AgentExecutionInputV1,
};
use crate::toolkits::{ToolAdmissionPolicy, ToolAdmissionPolicyErrorCode};

pub const AGENT_INPUT_SCHEMA_REVISION: &str = "elitea.runtime.agent-execution-input.v1";
const MAX_AGENT_INPUT_BYTES: usize = 8 * 1024 * 1024;
const MAX_JSON_VALUE_BYTES: usize = 256 * 1024;
const MAX_JSON_DEPTH: usize = 64;
const MAX_JSON_STRING_BYTES: usize = 64 * 1024;

/// Decode only the canonical protobuf representation admitted by protocol v1.
///
/// This rejects unknown fields, duplicate scalar encodings, explicit default
/// encodings and non-canonical field ordering because decoding and canonical
/// re-encoding must reproduce the exact fetched bytes.
///
/// # Errors
///
/// Returns [`AgentProtocolError::ResourceExhausted`] for an empty or oversized
/// payload and [`AgentProtocolError::InvalidInput`] for malformed,
/// non-canonical, or unsupported input.
pub fn parse_agent_execution_input(
    raw: &[u8],
) -> Result<AgentExecutionInputV1, AgentProtocolError> {
    if raw.is_empty() || raw.len() > MAX_AGENT_INPUT_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent execution input exceeds its limit",
        ));
    }
    let message = AgentExecutionInputV1::decode(raw)
        .map_err(|_| AgentProtocolError::InvalidInput("the agent execution input is malformed"))?;
    if message.encode_to_vec() != raw {
        return Err(AgentProtocolError::InvalidInput(
            "the agent execution input is not canonical protocol v1",
        ));
    }
    if message.schema_revision != AGENT_INPUT_SCHEMA_REVISION {
        return Err(AgentProtocolError::InvalidInput(
            "the agent execution input revision is not supported",
        ));
    }
    // FU-250: references are Main's to resolve; a materialized input never
    // carries one, so an input that still does is refused, not ignored.
    if !message.mcp_token_references.is_empty() {
        return Err(AgentProtocolError::InvalidInput(
            "the agent input carries unresolved credential references",
        ));
    }
    Ok(message)
}

/// Validate the input as [`request_from`] does, and also return its
/// admitted unattended grant (ADR-0045). An absent grant answers `None`.
///
/// # Errors
///
/// Returns the same errors as [`request_from`].
pub(crate) fn request_and_grant_from(
    message: AgentExecutionInputV1,
    kind: AgentExecutionKind,
    binding: AgentInputBinding,
) -> Result<(AgentExecutionRequest, Option<UnattendedGrant>), AgentProtocolError> {
    let grant = grant_from_input(&message)?;
    let request = request_from(message, kind, binding)?;
    Ok((request, grant))
}

#[cfg(test)]
mod credential_reference_tests {

    use super::*;
    use crate::protocol::elitea::runtime::v1::McpTokenReferenceV1;

    /// FU-250: a materialized input never carries a credential reference; one
    /// that still does is refused, so a Worker never runs with a reference it
    /// cannot resolve.
    #[test]
    fn an_input_with_unresolved_credential_references_is_refused() {
        let mut message = AgentExecutionInputV1 {
            schema_revision: AGENT_INPUT_SCHEMA_REVISION.to_owned(),
            ..AgentExecutionInputV1::default()
        };
        assert!(parse_agent_execution_input(&message.encode_to_vec()).is_ok());
        message.mcp_token_references.push(McpTokenReferenceV1 {
            resource: "https://mcp.example.invalid/".to_owned(),
            reference: "a".repeat(43),
            revision: 1,
            toolkit_id: 7,
        });
        let error = parse_agent_execution_input(&message.encode_to_vec())
            .expect_err("an unresolved reference is refused");
        assert!(
            error
                .to_string()
                .contains("unresolved credential references"),
            "{error}"
        );
    }
}

#[cfg(test)]
mod chat_history_ref_tests {
    use super::chat_history_refs;
    use crate::protocol::elitea::runtime::v1::ChatHistoryRefV1;

    fn reference(id: &str) -> ChatHistoryRefV1 {
        ChatHistoryRefV1 {
            message_id: id.to_owned(),
            readers_own: true,
        }
    }

    /// ADR-0050 decision 9c: none, or one valid uuid per history entry.
    #[test]
    fn chat_history_refs_name_every_entry_or_none() {
        assert_eq!(chat_history_refs(&[], 3).expect("none"), Vec::new());
        let id = "00000000-0000-4000-8000-0000000000AB";
        let refs = chat_history_refs(&[reference(id)], 1).expect("one");
        assert_eq!(refs[0].message_id, id.to_ascii_lowercase());
        assert!(refs[0].readers_own);
        assert!(chat_history_refs(&[reference(id)], 2).is_err());
        assert!(chat_history_refs(&[reference("not-a-uuid")], 1).is_err());
    }

    /// Main's contextview test pins the same vector.
    #[test]
    fn context_view_digest_matches_main() {
        assert_eq!(
            super::context_view_digest("since_joined", 8192, b"[]", &[]),
            "98c87e67997f565a4d27a3a984ac5d8e9aa3353c0457a2222ac27566fd08b5dc"
        );
    }

    /// A view whose digest, mode or window is not the history's is refused;
    /// no view is the input from before the view.
    #[test]
    fn context_view_is_checked_against_the_history() {
        use crate::protocol::elitea::runtime::v1::{
            AgentExecutionInputV1, ContextViewV1, ModelContextLimitsV1,
        };
        let id = "00000000-0000-4000-8000-0000000000ab";
        let mut message = AgentExecutionInputV1 {
            chat_history: br#"[{"role":"user","content":"hi"}]"#.to_vec(),
            chat_history_refs: vec![reference(id)],
            model_context_limits: Some(ModelContextLimitsV1 {
                context_window_tokens: 32000,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(super::check_context_view(&message).is_ok());
        let digest = super::context_view_digest(
            "full",
            32000,
            &message.chat_history,
            &message.chat_history_refs,
        );
        message.context_view = Some(ContextViewV1 {
            history: "full".to_owned(),
            window_tokens: 32000,
            digest: digest.clone(),
        });
        assert!(super::check_context_view(&message).is_ok());

        let mut widened = message.clone();
        widened.chat_history =
            br#"[{"role":"user","content":"hi"},{"role":"user","content":"x"}]"#.to_vec();
        assert!(super::check_context_view(&widened).is_err());
        let mut other_window = message.clone();
        if let Some(limits) = other_window.model_context_limits.as_mut() {
            limits.context_window_tokens = 8192;
        }
        assert!(super::check_context_view(&other_window).is_err());
        let mut other_mode = message.clone();
        if let Some(view) = other_mode.context_view.as_mut() {
            view.history = "everything".to_owned();
        }
        assert!(super::check_context_view(&other_mode).is_err());
        let mut flipped = message;
        flipped.chat_history_refs[0].readers_own = false;
        assert!(super::check_context_view(&flipped).is_err());
    }
}

#[cfg(test)]
mod recalled_memory_tests {
    use super::recalled_memory;
    use crate::protocol::elitea::runtime::v1::RecalledMemoryEntryV1;

    fn entry(id: &str, content: &str) -> RecalledMemoryEntryV1 {
        RecalledMemoryEntryV1 {
            entry_id: id.to_owned(),
            content: content.to_owned(),
        }
    }

    /// #1826: each recalled entry is a positive decimal id, named once,
    /// with the content of one stored entry.
    #[test]
    fn recalled_memory_is_checked() {
        assert_eq!(recalled_memory(&[]).expect("none"), Vec::new());
        let parsed = recalled_memory(&[entry("7", "a"), entry("42", "b")]).expect("two");
        assert_eq!(parsed.len(), 2);
        assert_eq!(parsed[1].entry_id, "42");
        assert_eq!(parsed[1].content, "b");
        for bad in [
            vec![entry("", "a")],
            vec![entry("07", "a")],
            vec![entry("m-1", "a")],
            vec![entry("7", "")],
            vec![entry("7", &"x".repeat(2049))],
            vec![entry("7", "a"), entry("7", "b")],
            (1..=51).map(|id| entry(&id.to_string(), "a")).collect(),
        ] {
            assert!(recalled_memory(&bad).is_err(), "{bad:?}");
        }
    }
}

#[cfg(test)]
mod tool_envelope_tests {
    use super::tool_envelope;
    use crate::agents::AgentProtocolError;
    use crate::protocol::elitea::runtime::v1::{AgentToolEnvelopeV1, AgentToolLimitV1};
    use elitea_agent_runtime::tool_envelope::ToolRef;

    #[test]
    fn the_envelope_decodes_and_narrows() {
        let decoded = tool_envelope(&AgentToolEnvelopeV1 {
            tool_limits: vec![AgentToolLimitV1 {
                tool: "github/create_issue".to_owned(),
                max_calls: 3,
            }],
            max_tool_limit: 2,
            subagent_tools: vec!["github/*".to_owned()],
            subagent_tools_set: true,
            ..AgentToolEnvelopeV1::default()
        })
        .expect("a valid envelope");
        let create = ToolRef {
            toolkit_name: "gh",
            toolkit_type: Some("github"),
            tool_name: "create_issue",
        };
        assert_eq!(decoded.limit_for(&create), Some(2));
        assert!(decoded.admits_subagent(&create, false));
        assert!(!decoded.admits_unattended(&create, false));
    }

    #[test]
    fn a_malformed_envelope_is_refused() {
        for envelope in [
            AgentToolEnvelopeV1 {
                tool_limits: vec![AgentToolLimitV1 {
                    tool: "github".to_owned(),
                    max_calls: 3,
                }],
                ..AgentToolEnvelopeV1::default()
            },
            AgentToolEnvelopeV1 {
                tool_limits: vec![AgentToolLimitV1 {
                    tool: "github/x".to_owned(),
                    max_calls: 0,
                }],
                ..AgentToolEnvelopeV1::default()
            },
            AgentToolEnvelopeV1 {
                unattended_tools: vec!["github/x".to_owned()],
                ..AgentToolEnvelopeV1::default()
            },
        ] {
            assert!(matches!(
                tool_envelope(&envelope),
                Err(AgentProtocolError::InvalidInput(_))
            ));
        }
    }
}

#[cfg(test)]
mod agent_floor_tests {
    use super::agent_floor;
    use crate::agents::request::{AgentFloorPeer, FloorCause};
    use crate::protocol::elitea::runtime::v1::{AgentFloorPeerV1, AgentFloorV1};

    fn peer(id: i64) -> AgentFloorPeerV1 {
        AgentFloorPeerV1 {
            participant_id: id,
            name: format!("agent-{id}"),
            role: "reviewer".to_owned(),
        }
    }

    fn floor(cause: &str, peers: Vec<AgentFloorPeerV1>) -> AgentFloorV1 {
        AgentFloorV1 {
            cause: cause.to_owned(),
            peers,
            unattended_tools_only: true,
        }
    }

    #[test]
    fn agent_floor_decodes_both_causes() {
        let decoded = agent_floor(&floor("agent_mention", vec![peer(7), peer(9)])).expect("ok");
        assert_eq!(decoded.cause, FloorCause::AgentMention);
        assert!(decoded.unattended_tools_only);
        assert_eq!(
            decoded.peers[0],
            AgentFloorPeer {
                participant_id: 7,
                name: "agent-7".to_owned(),
                role: "reviewer".to_owned(),
            }
        );
        let human = agent_floor(&floor("human_message", Vec::new())).expect("ok");
        assert_eq!(human.cause, FloorCause::HumanMessage);
        assert_eq!(human.peers, Vec::new());
        // ADR-0050 decision 2: a heartbeat act binds only unattended tools.
        let heartbeat = agent_floor(&floor("heartbeat", Vec::new())).expect("ok");
        assert_eq!(heartbeat.cause, FloorCause::Heartbeat);
        assert!(heartbeat.unattended_tools_only);
        assert!(agent_floor(&floor("cron", Vec::new())).is_err());
    }

    #[test]
    fn agent_floor_absent_is_none() {
        let message = crate::protocol::elitea::runtime::v1::AgentExecutionInputV1::default();
        assert!(
            message
                .floor
                .as_ref()
                .map(agent_floor)
                .transpose()
                .expect("none")
                .is_none()
        );
    }

    #[test]
    fn agent_floor_refuses_malformed_input() {
        assert!(agent_floor(&floor("", vec![peer(1)])).is_err());
        assert!(agent_floor(&floor("timer", vec![peer(1)])).is_err());
        assert!(agent_floor(&floor("human_message", (1..=17).map(peer).collect())).is_err());
        assert!(agent_floor(&floor("human_message", (1..=16).map(peer).collect())).is_ok());
        assert!(agent_floor(&floor("human_message", vec![peer(0)])).is_err());
        assert!(agent_floor(&floor("human_message", vec![peer(-3)])).is_err());
        assert!(agent_floor(&floor("human_message", vec![peer(4), peer(4)])).is_err());
        let mut long_name = peer(2);
        long_name.name = "n".repeat(257);
        assert!(agent_floor(&floor("human_message", vec![long_name])).is_err());
        let mut long_role = peer(2);
        long_role.role = "r".repeat(257);
        assert!(agent_floor(&floor("human_message", vec![long_role])).is_err());
        let mut edge = peer(2);
        edge.name = "n".repeat(256);
        assert!(agent_floor(&floor("human_message", vec![edge])).is_ok());
    }
}

/// Validate every semantic field and bind it to the exact materialized input.
///
/// # Errors
///
/// Returns [`AgentProtocolError::InvalidInput`] for a malformed binding,
/// semantic shape, or invariant. Bounded JSON input may also return
/// [`AgentProtocolError::ResourceExhausted`].
#[allow(clippy::too_many_lines)] // A linear field projection is easier to audit against the proto.
pub fn request_from(
    message: AgentExecutionInputV1,
    kind: AgentExecutionKind,
    binding: AgentInputBinding,
) -> Result<AgentExecutionRequest, AgentProtocolError> {
    validate_binding(&binding)?;
    // A malformed unattended grant refuses the input on every path, also on a
    // path that does not use the grant.
    grant_from_input(&message)?;

    let llm = json_object(&message.llm, "the agent llm must be an object")?;
    check_context_view(&message)?;
    // History is data-plane content. Let the model context policy compact it
    // after admission; do not apply the smaller control-field budget here.
    let Value::Array(chat_history) = parse_history(&message.chat_history)
        .map_err(|error| error.in_input_field(InputLimitField::ChatHistory))?
    else {
        return Err(AgentProtocolError::InvalidInput(
            "the agent chat history must be a list",
        ));
    };
    let chat_len = chat_history.len();
    let user_input = match parse_user_input(&message.user_input)
        .map_err(|error| error.in_input_field(InputLimitField::UserMessage))?
    {
        Value::String(value) => UserInput::Text(value),
        Value::Array(value) => UserInput::ContentBlocks(value),
        _ => {
            return Err(AgentProtocolError::InvalidInput(
                "the agent user input must be text or content blocks",
            ));
        }
    };
    let tools = json_list(&message.tools, "the agent tools must be a list")
        .map_err(|error| error.in_input_field(InputLimitField::ToolConfiguration))?;
    let application = application_object(&message.application)
        .map_err(|error| error.in_input_field(InputLimitField::AgentSettings))?;
    let internal_tools = string_list(
        &message.internal_tools,
        "the agent internal tools must contain only strings",
    )?;
    let mcp_tokens = json_object(
        &message.mcp_tokens,
        "the agent MCP tokens must be an object",
    )?;
    let ignored_mcp_servers = json_list(
        &message.ignored_mcp_servers,
        "the agent ignored MCP servers must be a list",
    )?;
    let user_declined_mcp_servers = json_list(
        &message.user_declined_mcp_servers,
        "the agent declined MCP servers must be a list",
    )?;
    let hitl_decisions = json_list(
        &message.hitl_decisions,
        "the agent HITL decisions must be a list",
    )?;
    let meta = json_object(&message.meta, "the agent metadata must be an object")?;
    let context_settings = json_object(
        &message.context_settings,
        "the agent context settings must be an object",
    )?;
    let invoked_skills = json_list(
        &message.invoked_skills,
        "the agent invoked skills must be a list",
    )?;
    let applied_skills = json_list(
        &message.applied_skills,
        "the agent applied skills must be a list",
    )?;
    let attached_skills = json_list(
        &message.attached_skills,
        "the agent attached skills must be a list",
    )?;
    let input_attachments = json_list(
        &message.input_attachments,
        "the agent input attachments must be a list",
    )?;
    let parallel_reconcile = optional_json_object(
        &message.parallel_reconcile,
        "the agent parallel reconcile must be an object or null",
    )?;
    let parallel_terminal_errors = json_list(
        &message.parallel_terminal_errors,
        "the agent parallel terminal errors must be a list",
    )?;
    let next_input_suggestion = next_input_suggestion_policy(&message.next_input_suggestion)?;
    let toolkit_guardrails = toolkit_guardrails_policy(&message.toolkit_guardrails)?;
    let truncated_content = optional_continuation_text(
        &message.truncated_content,
        "the truncated agent content must be text",
    )?;

    let steps_limit = match message.steps_limit {
        Some(value) if value > 0 => Some(value.cast_unsigned()),
        Some(_) => {
            return Err(AgentProtocolError::InvalidInput(
                "the agent step limit must be positive",
            ));
        }
        None => None,
    };
    if message.hitl_resume && message.hitl_action.is_none() && hitl_decisions.is_empty() {
        return Err(AgentProtocolError::InvalidInput(
            "a HITL resume decision is required",
        ));
    }
    match kind {
        AgentExecutionKind::Application => validate_application_identity(&application)?,
        AgentExecutionKind::Adhoc if model_name(&llm).is_none() => {
            return Err(AgentProtocolError::InvalidInput(
                "the ad-hoc agent model is required",
            ));
        }
        AgentExecutionKind::Adhoc => {}
    }

    Ok(AgentExecutionRequest {
        kind,
        binding,
        payload: AgentExecutionPayload {
            llm,
            chat_history,
            user_input,
            thread_id: message.thread_id,
            checkpoint_id: message.checkpoint_id,
            debug: message.debug,
            tools,
            application,
            internal_tools,
            steps_limit,
            mcp_tokens,
            ignored_mcp_servers,
            user_declined_mcp_servers,
            should_continue: message.should_continue,
            hitl_resume: message.hitl_resume,
            hitl_action: message.hitl_action,
            hitl_value: message.hitl_value,
            hitl_decisions,
            execution_generation: message.execution_generation,
            is_regenerate: message.is_regenerate,
            meta,
            conversation_id: message.conversation_id,
            persona: if message.persona.is_empty() {
                "generic".to_owned()
            } else {
                message.persona
            },
            context_settings,
            supports_vision: message.supports_vision,
            return_chat_history: message.return_chat_history,
            invoked_skills,
            applied_skills,
            auto_approve_sensitive_actions: message.auto_approve_sensitive_actions,
            attached_skills,
            input_attachments,
            parallel_reconcile,
            parallel_terminal_errors,
            exception_handling_enabled: message.exception_handling_enabled,
            debug_mode: message.debug_mode,
            next_input_suggestion,
            toolkit_guardrails,
            truncated_content,
            model_context_limits: message.model_context_limits.map(model_context_limits),
            summary_model: message
                .summary_model
                .map(|summary| {
                    Ok(super::request::SummaryModelSnapshot {
                        llm_settings: json_object(
                            &summary.llm_settings,
                            "the summary model settings must be an object",
                        )?,
                        model_context_limits: model_context_limits(
                            summary.model_context_limits.ok_or(
                                AgentProtocolError::InvalidInput(
                                    "the summary model limits are required",
                                ),
                            )?,
                        ),
                        model_reasoning: summary
                            .model_reasoning
                            .as_ref()
                            .map(model_reasoning)
                            .transpose()?,
                    })
                })
                .transpose()?,
            model_reasoning: message
                .model_reasoning
                .as_ref()
                .map(model_reasoning)
                .transpose()?,
            platform_preamble: message
                .platform_preamble
                .map(platform_preamble_snapshot)
                .transpose()?,
            chat_history_refs: chat_history_refs(&message.chat_history_refs, chat_len)?,
            floor: message.floor.as_ref().map(agent_floor).transpose()?,
            tool_envelope: message
                .tool_envelope
                .as_ref()
                .map(tool_envelope)
                .transpose()?,
            current_message_id: message
                .current_message_id
                .as_deref()
                .map(current_message_id)
                .transpose()?,
            recalled_memory: recalled_memory(&message.recalled_memory)?,
            project_context: message.project_context.map(|context| {
                super::request::ProjectContextSnapshot {
                    id: context.id,
                    revision: context.revision,
                    scope: context.scope,
                    content: context.content,
                    activation_description: context.activation_description,
                }
            }),
        },
    })
}

/// ADR-0050 decision 9 (#1609): Main builds `chat_history` and its
/// references as this reader's view and says how in `context_view`. The
/// worker recomputes the view's digest over the exact bytes it received and
/// refuses an input whose history, references, mode or window are not the
/// ones Main described: a view is never widened or swapped on the way. An
/// input without a view is one from before the view and is read as before.
fn check_context_view(message: &AgentExecutionInputV1) -> Result<(), AgentProtocolError> {
    let Some(view) = message.context_view.as_ref() else {
        return Ok(());
    };
    if !matches!(
        view.history.as_str(),
        "full" | "since_joined" | "addressed_only" | "none"
    ) {
        return Err(AgentProtocolError::InvalidInput(
            "the agent context view mode is unknown",
        ));
    }
    let window = message
        .model_context_limits
        .as_ref()
        .map_or(0, |limits| limits.context_window_tokens);
    if view.window_tokens != window
        || view.digest
            != context_view_digest(
                &view.history,
                view.window_tokens,
                &message.chat_history,
                &message.chat_history_refs,
            )
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent context view does not match its history",
        ));
    }
    Ok(())
}

/// Main's `contextview.Digest`: SHA-256 over the domain, the mode, the
/// window, the history bytes and each reference, every variable-length field
/// prefixed with its big-endian 64-bit length, as lowercase hex.
fn context_view_digest(
    history: &str,
    window_tokens: u32,
    chat_history: &[u8],
    refs: &[crate::protocol::elitea::runtime::v1::ChatHistoryRefV1],
) -> String {
    use sha2::Digest as _;
    fn field(hash: &mut sha2::Sha256, value: &[u8]) {
        hash.update((value.len() as u64).to_be_bytes());
        hash.update(value);
    }
    let mut hash = sha2::Sha256::new();
    field(&mut hash, b"elitea.context-view.v1");
    field(&mut hash, history.as_bytes());
    hash.update(window_tokens.to_be_bytes());
    field(&mut hash, chat_history);
    hash.update((refs.len() as u64).to_be_bytes());
    for reference in refs {
        field(&mut hash, reference.message_id.as_bytes());
        hash.update([u8::from(reference.readers_own)]);
    }
    hash.finalize()
        .iter()
        .fold(String::with_capacity(64), |mut out, byte| {
            use std::fmt::Write as _;
            let _ = write!(out, "{byte:02x}");
            out
        })
}

/// The most peers one floor names (ADR-0050 decision 11).
const MAX_FLOOR_PEERS: usize = 16;
/// The longest peer name or role, in bytes.
const MAX_FLOOR_PEER_TEXT: usize = 256;

/// ADR-0050 decision 11 (#1611): the floor Main hands this invocation. A
/// cause this worker does not know, too many peers, a non-positive or
/// repeated participant, or an oversized name or role is refused.
fn agent_floor(
    floor: &crate::protocol::elitea::runtime::v1::AgentFloorV1,
) -> Result<super::request::AgentFloor, AgentProtocolError> {
    use super::request::{AgentFloor, AgentFloorPeer, FloorCause};
    let cause = match floor.cause.as_str() {
        "human_message" => FloorCause::HumanMessage,
        "agent_mention" => FloorCause::AgentMention,
        "heartbeat" => FloorCause::Heartbeat,
        _ => {
            return Err(AgentProtocolError::InvalidInput(
                "the agent floor cause is unknown",
            ));
        }
    };
    if floor.peers.len() > MAX_FLOOR_PEERS {
        return Err(AgentProtocolError::InvalidInput(
            "the agent floor names too many peers",
        ));
    }
    let mut seen = HashSet::with_capacity(floor.peers.len());
    let mut peers = Vec::with_capacity(floor.peers.len());
    for peer in &floor.peers {
        if peer.participant_id <= 0 || !seen.insert(peer.participant_id) {
            return Err(AgentProtocolError::InvalidInput(
                "the agent floor peers must be distinct positive participants",
            ));
        }
        if peer.name.len() > MAX_FLOOR_PEER_TEXT || peer.role.len() > MAX_FLOOR_PEER_TEXT {
            return Err(AgentProtocolError::InvalidInput(
                "the agent floor peer name or role is too long",
            ));
        }
        peers.push(AgentFloorPeer {
            participant_id: peer.participant_id,
            name: peer.name.clone(),
            role: peer.role.clone(),
        });
    }
    Ok(AgentFloor {
        cause,
        peers,
        unattended_tools_only: floor.unattended_tools_only,
    })
}

/// ADR-0050 decision 3 (#1613): the root agent's tool envelope Main froze.
/// A malformed key, a limit out of range or an oversized list is refused.
fn tool_envelope(
    envelope: &crate::protocol::elitea::runtime::v1::AgentToolEnvelopeV1,
) -> Result<elitea_agent_runtime::tool_envelope::ToolEnvelope, AgentProtocolError> {
    use elitea_agent_runtime::tool_envelope::{ToolEnvelope, ToolEnvelopeError, ToolEnvelopeInput};
    ToolEnvelope::from_input(&ToolEnvelopeInput {
        tool_limits: envelope
            .tool_limits
            .iter()
            .map(|limit| (limit.tool.clone(), limit.max_calls))
            .collect(),
        max_tool_limit: envelope.max_tool_limit,
        unattended_tools: envelope.unattended_tools.clone(),
        unattended_tools_set: envelope.unattended_tools_set,
        unattended_tools_allowlist: envelope.unattended_tools_allowlist.clone(),
        subagent_tools: envelope.subagent_tools.clone(),
        subagent_tools_set: envelope.subagent_tools_set,
    })
    .map_err(|error| match error {
        ToolEnvelopeError::TooLarge => {
            AgentProtocolError::ResourceExhausted("the agent tool envelope exceeds its limit")
        }
        ToolEnvelopeError::Invalid(_) => {
            AgentProtocolError::InvalidInput("the agent tool envelope is malformed")
        }
    })
}

/// #1831: this turn's question, a uuid, in the form the history's
/// references take (lowercase, hyphenated), so one message has one short id.
fn current_message_id(id: &str) -> Result<String, AgentProtocolError> {
    uuid::Uuid::try_parse(id)
        .map(|id| id.hyphenated().to_string())
        .map_err(|_| AgentProtocolError::InvalidInput("the agent current message id is not a uuid"))
}

/// The most entries Main recalls into one turn (`agentmemory.MaxRecallEntries`).
const MAX_RECALLED_MEMORY: usize = 50;
/// The longest stored entry (`agentmemory.MaxContentBytes`).
const MAX_RECALLED_MEMORY_BYTES: usize = 2048;

/// #1826: the entries Main recalled into the instructions. Each id is a
/// positive decimal number of at most 19 digits, named once; each content
/// is at most one stored entry long.
fn recalled_memory(
    entries: &[crate::protocol::elitea::runtime::v1::RecalledMemoryEntryV1],
) -> Result<Vec<super::request::RecalledMemoryEntry>, AgentProtocolError> {
    let invalid =
        || AgentProtocolError::InvalidInput("the agent recalled memory entries are invalid");
    if entries.len() > MAX_RECALLED_MEMORY {
        return Err(invalid());
    }
    let mut seen = std::collections::BTreeSet::new();
    entries
        .iter()
        .map(|entry| {
            let id = entry.entry_id.as_str();
            let numeric = !id.is_empty()
                && id.len() <= 19
                && !id.starts_with('0')
                && id.bytes().all(|byte| byte.is_ascii_digit());
            if !numeric
                || entry.content.is_empty()
                || entry.content.len() > MAX_RECALLED_MEMORY_BYTES
                || !seen.insert(id)
            {
                return Err(invalid());
            }
            Ok(super::request::RecalledMemoryEntry {
                entry_id: id.to_owned(),
                content: entry.content.clone(),
            })
        })
        .collect()
}

/// ADR-0050 decision 9c: one reference per history entry, each a uuid, or
/// none at all (an input from before field 71).
fn chat_history_refs(
    refs: &[crate::protocol::elitea::runtime::v1::ChatHistoryRefV1],
    history_len: usize,
) -> Result<Vec<super::request::ChatHistoryRef>, AgentProtocolError> {
    if refs.is_empty() {
        return Ok(Vec::new());
    }
    if refs.len() != history_len
        || refs
            .iter()
            .any(|entry| uuid::Uuid::try_parse(&entry.message_id).is_err())
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent chat history references must name every entry",
        ));
    }
    Ok(refs
        .iter()
        .map(|entry| super::request::ChatHistoryRef {
            message_id: entry.message_id.to_ascii_lowercase(),
            readers_own: entry.readers_own,
        })
        .collect())
}

fn model_context_limits(
    limits: crate::protocol::elitea::runtime::v1::ModelContextLimitsV1,
) -> super::request::ModelContextLimits {
    super::request::ModelContextLimits {
        context_window_tokens: limits.context_window_tokens,
        max_output_tokens: limits.max_output_tokens,
        context_window_fallback: limits.context_window_fallback,
        max_output_fallback: limits.max_output_fallback,
        max_input_tokens: limits.max_input_tokens,
    }
}

/// The catalogue's thinking data. An unknown control kind is refused: a
/// newer catalogue value this worker cannot translate is never sent as
/// another form.
fn model_reasoning(
    reasoning: &crate::protocol::elitea::runtime::v1::ModelReasoningV1,
) -> Result<super::request::ModelReasoning, AgentProtocolError> {
    super::request::ModelReasoning::parse(&reasoning.control, reasoning.tool_steps)
        .map(|parsed| parsed.with_off_with_tools(reasoning.off_with_tools))
        .ok_or(AgentProtocolError::InvalidInput(
            "the model reasoning control is not known",
        ))
}

fn validate_binding(binding: &AgentInputBinding) -> Result<(), AgentProtocolError> {
    if binding.input_bundle_id.is_empty()
        || binding.request_entry_id.is_empty()
        || binding.request_immutable_version.is_empty()
        || binding.input_bundle_digest.iter().all(|byte| *byte == 0)
        || binding.request_content_digest.iter().all(|byte| *byte == 0)
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent execution input binding is malformed",
        ));
    }
    Ok(())
}

pub(crate) fn parse_json_value(raw: &[u8]) -> Result<Value, AgentProtocolError> {
    parse_bounded_json_value(raw, MAX_JSON_VALUE_BYTES)
}

// User text is data-plane content. Escaped JSON may exceed the decoded
// text limit, but the complete encoded execution input stays bounded.
fn parse_user_input(raw: &[u8]) -> Result<Value, AgentProtocolError> {
    if raw.is_empty() || raw.len() > MAX_AGENT_INPUT_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent user input exceeds the input limit",
        ));
    }
    let value = serde_json::from_slice(raw).map_err(|_| malformed_json())?;
    JsonLimits::new(raw).validate_scope(JsonTextScope::UserText)?;
    Ok(value)
}

fn parse_history(raw: &[u8]) -> Result<Value, AgentProtocolError> {
    if raw.is_empty() || raw.len() > MAX_AGENT_INPUT_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent history exceeds the input limit",
        ));
    }
    let value = serde_json::from_slice(raw).map_err(|_| malformed_json())?;
    JsonLimits {
        raw,
        cursor: 0,
        max_string_bytes: MAX_AGENT_INPUT_BYTES,
    }
    .validate()?;
    Ok(value)
}

pub(crate) fn parse_bounded_json_value(
    raw: &[u8],
    maximum: usize,
) -> Result<Value, AgentProtocolError> {
    if raw.is_empty() || raw.len() > maximum {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent JSON input exceeds the approved limit",
        ));
    }
    let value = serde_json::from_slice(raw)
        .map_err(|_| AgentProtocolError::InvalidInput("the agent JSON input is malformed"))?;
    JsonLimits::new(raw).validate()?;
    Ok(value)
}

// Only instruction values use the content budget. Identifiers, keys, and
// arbitrary metadata retain the control-string limit.
fn application_object(raw: &[u8]) -> Result<Map<String, Value>, AgentProtocolError> {
    if raw.is_empty() || raw.len() > MAX_AGENT_INPUT_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the agent application snapshot exceeds the input limit",
        ));
    }
    let value = serde_json::from_slice(raw).map_err(|_| malformed_json())?;
    JsonLimits::new(raw).validate_scope(JsonTextScope::Application)?;
    match value {
        Value::Object(value) => Ok(value),
        _ => Err(AgentProtocolError::InvalidInput(
            "the agent application must be an object",
        )),
    }
}

#[derive(Clone, Copy)]
enum JsonTextScope {
    Control,
    Application,
    Version,
    Instruction,
    UserText,
}

impl JsonTextScope {
    fn member(self, key: &str) -> Self {
        match (self, key) {
            (Self::Application, "version_details") => Self::Version,
            (Self::Application | Self::Version, "instructions") => Self::Instruction,
            _ => Self::Control,
        }
    }
}

fn json_object(
    raw: &[u8],
    shape_error: &'static str,
) -> Result<Map<String, Value>, AgentProtocolError> {
    match parse_json_value(raw)? {
        Value::Object(value) => Ok(value),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn optional_json_object(
    raw: &[u8],
    shape_error: &'static str,
) -> Result<Option<Map<String, Value>>, AgentProtocolError> {
    match parse_json_value(raw)? {
        Value::Null => Ok(None),
        Value::Object(value) => Ok(Some(value)),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn optional_continuation_text(
    raw: &[u8],
    shape_error: &'static str,
) -> Result<Option<String>, AgentProtocolError> {
    if raw.is_empty() {
        return Ok(None);
    }
    if raw.len() > MAX_AGENT_INPUT_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the continuation input exceeds the approved limit",
        ));
    }
    // Decode a scalar directly, without allocating arbitrary nested JSON values.
    let value: String =
        serde_json::from_slice(raw).map_err(|_| AgentProtocolError::InvalidInput(shape_error))?;
    if value.len() > super::request::MAX_OUTPUT_CONTINUATION_BYTES {
        return Err(AgentProtocolError::ResourceExhausted(
            "the continuation text exceeds the approved limit",
        ));
    }
    Ok(Some(value))
}

fn json_list(raw: &[u8], shape_error: &'static str) -> Result<Vec<Value>, AgentProtocolError> {
    match parse_json_value(raw)? {
        Value::Array(value) => Ok(value),
        _ => Err(AgentProtocolError::InvalidInput(shape_error)),
    }
}

fn string_list(raw: &[u8], shape_error: &'static str) -> Result<Vec<String>, AgentProtocolError> {
    json_list(raw, shape_error)?
        .into_iter()
        .map(|value| match value {
            Value::String(value) => Ok(value),
            _ => Err(AgentProtocolError::InvalidInput(shape_error)),
        })
        .collect()
}

fn validate_application_identity(
    application: &Map<String, Value>,
) -> Result<(), AgentProtocolError> {
    let ids_present = application.get("id").is_some_and(is_positive_integer)
        && application
            .get("version_id")
            .is_some_and(is_positive_integer);
    if !ids_present
        && !application
            .get("version_details")
            .is_some_and(Value::is_object)
    {
        return Err(AgentProtocolError::InvalidInput(
            "the configured application identity is required",
        ));
    }
    Ok(())
}

fn is_positive_integer(value: &Value) -> bool {
    let Value::Number(value) = value else {
        return false;
    };
    let text = value.to_string();
    !text.starts_with('-')
        && text != "0"
        && !text.bytes().any(|byte| matches!(byte, b'.' | b'e' | b'E'))
}

fn model_name(llm: &Map<String, Value>) -> Option<&str> {
    llm.get("kwargs")?
        .as_object()?
        .get("model")?
        .as_str()
        .filter(|value| !value.is_empty())
}

fn next_input_suggestion_policy(
    raw: &[u8],
) -> Result<NextInputSuggestionPolicy, AgentProtocolError> {
    if raw.is_empty() {
        return Ok(NextInputSuggestionPolicy::default());
    }
    let value = parse_json_value(raw)?;
    let Value::Object(value) = value else {
        if value.is_null() {
            return Ok(NextInputSuggestionPolicy::default());
        }
        return Err(next_input_suggestion_error());
    };
    if value.keys().any(|key| {
        !matches!(
            key.as_str(),
            "enabled" | "min_response_chars" | "timeout_seconds"
        )
    }) {
        return Err(next_input_suggestion_error());
    }
    let enabled = match value.get("enabled") {
        Some(Value::Bool(value)) => *value,
        None => false,
        Some(_) => return Err(next_input_suggestion_error()),
    };
    let min_response_chars = bounded_integer(value.get("min_response_chars"), 150, 100_000)?;
    let timeout_seconds = bounded_integer(value.get("timeout_seconds"), 15, 300)?;
    Ok(NextInputSuggestionPolicy {
        enabled,
        min_response_chars,
        timeout_seconds: timeout_seconds
            .try_into()
            .map_err(|_| next_input_suggestion_error())?,
    })
}

fn bounded_integer(
    value: Option<&Value>,
    default: u32,
    maximum: u32,
) -> Result<u32, AgentProtocolError> {
    match value {
        None => Ok(default),
        Some(Value::Number(value)) => value
            .as_u64()
            .and_then(|value| u32::try_from(value).ok())
            .filter(|value| (1..=maximum).contains(value))
            .ok_or_else(next_input_suggestion_error),
        Some(_) => Err(next_input_suggestion_error()),
    }
}

const fn next_input_suggestion_error() -> AgentProtocolError {
    AgentProtocolError::InvalidInput("the agent next input suggestion policy is invalid")
}

fn toolkit_guardrails_policy(raw: &[u8]) -> Result<Option<Map<String, Value>>, AgentProtocolError> {
    if raw.is_empty() {
        return Ok(None);
    }
    let value = parse_json_value(raw)?;
    let Value::Object(policy) = value else {
        if value.is_null() {
            return Ok(None);
        }
        return Err(toolkit_guardrails_error());
    };
    let runtime = Map::from_iter([("toolkit_security".to_owned(), Value::Object(policy.clone()))]);
    ToolAdmissionPolicy::from_runtime_config(&runtime).map_err(|error| match error.code() {
        ToolAdmissionPolicyErrorCode::InvalidConfiguration => toolkit_guardrails_error(),
        ToolAdmissionPolicyErrorCode::ResourceExhausted => AgentProtocolError::ResourceExhausted(
            "the agent toolkit guardrails exceed the approved limit",
        ),
    })?;
    Ok(Some(policy))
}

const fn toolkit_guardrails_error() -> AgentProtocolError {
    AgentProtocolError::InvalidInput("the agent toolkit guardrails are invalid")
}

/// A second, allocation-bounded structural pass catches duplicate object
/// members and applies Python-worker-compatible decoded string/depth limits.
/// `serde_json` performs the JSON grammar validation and preserves arbitrary
/// precision numbers; this pass only walks already-valid syntax.
struct JsonLimits<'a> {
    raw: &'a [u8],
    cursor: usize,
    max_string_bytes: usize,
}

impl<'a> JsonLimits<'a> {
    const fn new(raw: &'a [u8]) -> Self {
        Self {
            raw,
            cursor: 0,
            max_string_bytes: MAX_JSON_STRING_BYTES,
        }
    }

    fn validate(self) -> Result<(), AgentProtocolError> {
        self.validate_scope(JsonTextScope::Control)
    }

    fn validate_scope(mut self, scope: JsonTextScope) -> Result<(), AgentProtocolError> {
        self.skip_whitespace();
        self.value(0, scope)?;
        self.skip_whitespace();
        if self.cursor != self.raw.len() {
            return Err(malformed_json());
        }
        Ok(())
    }

    fn value(&mut self, depth: usize, scope: JsonTextScope) -> Result<(), AgentProtocolError> {
        if depth > MAX_JSON_DEPTH {
            return Err(AgentProtocolError::ResourceExhausted(
                "the agent JSON input exceeds the nesting limit",
            ));
        }
        self.skip_whitespace();
        match self.peek() {
            Some(b'{') => self.object(depth, scope),
            Some(b'[') => self.array(depth),
            Some(b'\"') => self
                .string(match scope {
                    JsonTextScope::Instruction => MAX_AGENT_INSTRUCTION_BYTES,
                    JsonTextScope::UserText => MAX_AGENT_USER_INPUT_BYTES,
                    _ => self.max_string_bytes,
                })
                .map(|_| ()),
            Some(b't') => self.literal(b"true"),
            Some(b'f') => self.literal(b"false"),
            Some(b'n') => self.literal(b"null"),
            Some(b'-' | b'0'..=b'9') => self.number(),
            _ => Err(malformed_json()),
        }
    }

    fn object(&mut self, depth: usize, scope: JsonTextScope) -> Result<(), AgentProtocolError> {
        self.cursor += 1;
        self.skip_whitespace();
        if self.take(b'}') {
            return Ok(());
        }
        let mut keys = HashSet::new();
        loop {
            let key = self.string(self.max_string_bytes)?;
            let member_scope = scope.member(&key);
            if !keys.insert(key) {
                return Err(AgentProtocolError::InvalidInput(
                    "the agent JSON input contains a duplicate member",
                ));
            }
            self.skip_whitespace();
            if !self.take(b':') {
                return Err(malformed_json());
            }
            self.value(depth + 1, member_scope)?;
            self.skip_whitespace();
            if self.take(b'}') {
                return Ok(());
            }
            if !self.take(b',') {
                return Err(malformed_json());
            }
            self.skip_whitespace();
        }
    }

    fn array(&mut self, depth: usize) -> Result<(), AgentProtocolError> {
        self.cursor += 1;
        self.skip_whitespace();
        if self.take(b']') {
            return Ok(());
        }
        loop {
            self.value(depth + 1, JsonTextScope::Control)?;
            self.skip_whitespace();
            if self.take(b']') {
                return Ok(());
            }
            if !self.take(b',') {
                return Err(malformed_json());
            }
            self.skip_whitespace();
        }
    }

    fn string(&mut self, maximum: usize) -> Result<String, AgentProtocolError> {
        let start = self.cursor;
        if !self.take(b'\"') {
            return Err(malformed_json());
        }
        let mut escaped = false;
        while let Some(byte) = self.peek() {
            self.cursor += 1;
            if escaped {
                escaped = false;
            } else if byte == b'\\' {
                escaped = true;
            } else if byte == b'\"' {
                let value: String = serde_json::from_slice(&self.raw[start..self.cursor])
                    .map_err(|_| malformed_json())?;
                if value.len() > maximum {
                    return Err(AgentProtocolError::ResourceExhausted(
                        "an agent JSON string exceeds the approved limit",
                    ));
                }
                return Ok(value);
            }
        }
        Err(malformed_json())
    }

    fn literal(&mut self, expected: &[u8]) -> Result<(), AgentProtocolError> {
        let end = self.cursor.saturating_add(expected.len());
        if self.raw.get(self.cursor..end) != Some(expected) {
            return Err(malformed_json());
        }
        self.cursor = end;
        Ok(())
    }

    fn number(&mut self) -> Result<(), AgentProtocolError> {
        let start = self.cursor;
        while self
            .peek()
            .is_some_and(|byte| !matches!(byte, b' ' | b'\t' | b'\r' | b'\n' | b',' | b']' | b'}'))
        {
            self.cursor += 1;
        }
        let raw = self
            .raw
            .get(start..self.cursor)
            .ok_or_else(malformed_json)?;
        if raw.iter().any(|byte| matches!(byte, b'.' | b'e' | b'E')) {
            let number = std::str::from_utf8(raw)
                .ok()
                .and_then(|value| value.parse::<f64>().ok());
            if !number.is_some_and(f64::is_finite) {
                return Err(AgentProtocolError::InvalidInput(
                    "the agent JSON input contains a non-finite number",
                ));
            }
        }
        Ok(())
    }

    fn skip_whitespace(&mut self) {
        while self
            .peek()
            .is_some_and(|byte| matches!(byte, b' ' | b'\t' | b'\r' | b'\n'))
        {
            self.cursor += 1;
        }
    }

    fn take(&mut self, expected: u8) -> bool {
        if self.peek() == Some(expected) {
            self.cursor += 1;
            true
        } else {
            false
        }
    }

    fn peek(&self) -> Option<u8> {
        self.raw.get(self.cursor).copied()
    }
}

const fn malformed_json() -> AgentProtocolError {
    AgentProtocolError::InvalidInput("the agent JSON input is malformed")
}

/// The platform preamble the input carries (ADR-0043 decision 3): a
/// revision, a text of at most 8 KiB without NUL, and the lowercase hex
/// SHA-256 of that text. A text that does not have its digest fails the
/// input closed.
fn platform_preamble_snapshot(
    snapshot: crate::protocol::elitea::runtime::v1::PlatformPreambleSnapshotV1,
) -> Result<super::request::PlatformPreambleSnapshot, AgentProtocolError> {
    use sha2::Digest as _;
    let digest = sha2::Sha256::digest(snapshot.text.as_bytes());
    let digest = digest
        .iter()
        .fold(String::with_capacity(64), |mut out, byte| {
            use std::fmt::Write as _;
            let _ = write!(out, "{byte:02x}");
            out
        });
    if snapshot.revision == 0
        || snapshot.text.is_empty()
        || snapshot.text.len() > elitea_agent_runtime::preamble::MAX_PLATFORM_PREAMBLE_BYTES
        || snapshot.text.contains('\0')
        || snapshot.sha256 != digest
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent platform preamble is malformed",
        ));
    }
    Ok(super::request::PlatformPreambleSnapshot {
        revision: snapshot.revision,
        sha256: snapshot.sha256,
        text: snapshot.text,
    })
}
