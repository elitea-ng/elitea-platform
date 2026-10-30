//! Fan-out stream frames (contract `fanout-interrupt-decisions-v1.md` §3.3,
//! §3.4, §4, §9): the `agent_interrupt_pending` card and the
//! `agent_fanout_member` lifecycle frame, plus the stable member identities
//! they carry.
//!
//! Every struct serializes its fields in UTF-8 key order, so the produced
//! JSON is the contract canonical form byte for byte (the conformance tests
//! compare against the schema fixtures). Frames carry display data the card
//! shows and nothing else: no checkpoint ids, tokens, headers or values.

use ring::digest;
use serde::Serialize;

use crate::protocol::elitea::runtime::v1::NodeEventV1;

pub(crate) const INTERRUPT_PENDING_EVENT: &str = "agent_interrupt_pending";
pub(crate) const FANOUT_MEMBER_EVENT: &str = "agent_fanout_member";
pub(crate) const FANOUT_PROGRESS_EVENT: &str = "agent_fanout_progress";
#[cfg_attr(
    not(test),
    allow(
        dead_code,
        reason = "coalesced progress frames are wired with Map (2a-3)"
    )
)]
const PROGRESS_SCHEMA: &str = "elitea.pipeline.fanout-progress.v1";
/// Text bytes one progress frame carries across all its members (v1.1 §9.1):
/// worst-case escaped JSON stays under the node-event bound.
#[cfg_attr(
    not(test),
    allow(
        dead_code,
        reason = "coalesced progress frames are wired with Map (2a-3)"
    )
)]
pub(crate) const MAX_PROGRESS_TEXT_BYTES: usize = 8192;
const CARD_SCHEMA: &str = "elitea.pipeline.fanout-interrupt-card.v1";
/// The internal event that carries one built frame to the projector. Only the
/// fan-out node stamps it; the projector emits the frame in the run's order.
pub(crate) const FANOUT_FRAME_METADATA_KEY: &str = "elitea.fanout.frame.v1";
pub(crate) const FANOUT_FRAME_AUTHOR: &str = "elitea-fanout";
/// One stamped frame's metadata, encoded (the node-event bound is 60 KiB).
const MAX_FRAME_METADATA_BYTES: usize = 48 * 1024;
const MEMBER_STATUS_SCHEMA: &str = "elitea.pipeline.fanout-member-status.v1";
const MEMBER_CALL_DOMAIN: &[u8] = b"elitea.graph.fanout-member-call.v1\0";
const ACTIVATION_LABEL_DOMAIN: &[u8] = b"elitea.graph.fanout-activation-label.v1\0";

/// `fanout_v1` grouping metadata (§3.4). Never a container, never content.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct FanoutMemberV1 {
    pub(crate) activation: String,
    pub(crate) kind: &'static str,
    pub(crate) member: String,
    pub(crate) node: String,
    pub(crate) ordinal: usize,
    pub(crate) total: usize,
}

/// One `parent_agent_path` tier.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct AgentPathTier {
    pub(crate) call_id: String,
    pub(crate) name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) sibling_ordinal: Option<usize>,
}

/// The three hierarchy fields every member-scoped frame carries.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct MemberHierarchy {
    /// Enclosing tiers first, the member tier last.
    pub(crate) path: Vec<AgentPathTier>,
}

impl MemberHierarchy {
    fn leaf(&self) -> Option<&AgentPathTier> {
        self.path.last()
    }
}

/// The closed `display` object of a card (§4).
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub(crate) struct CardDisplay {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) action_label: Option<String>,
    /// The worker's digest of the call's arguments BEFORE masking (the
    /// approval ledger evidence, ADR-0041 decision 5). `tool_args_json` is
    /// the masked text the card shows.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) argument_digest: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) edit_state_key: Option<String>,
    pub(crate) guardrail_type: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) interaction_type: Option<String>,
    pub(crate) message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) node_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) policy_message: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) server_url: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) tool_args_json: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) tool_call_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) tool_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) toolkit_name: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub(crate) toolkit_type: Option<String>,
}

/// The four card kinds and the actions each one offers (§5.1, schema).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CardKind {
    ToolGuard,
    HitlNode,
    AskUser,
    DelegatedAuth,
}

impl CardKind {
    const fn tag(self) -> &'static str {
        match self {
            Self::ToolGuard => "tool_guard",
            Self::HitlNode => "hitl_node",
            Self::AskUser => "ask_user",
            Self::DelegatedAuth => "delegated_auth",
        }
    }

    /// Whether `guardrail_type` belongs to this kind, and `action` to both.
    fn coherent(self, guardrail_type: &str, action: &str) -> bool {
        let guardrail = match self {
            Self::ToolGuard => guardrail_type == "sensitive_tool",
            Self::HitlNode => matches!(guardrail_type, "pipeline_hitl" | "pipeline_static"),
            Self::AskUser => guardrail_type == "clarifying_question",
            Self::DelegatedAuth => guardrail_type == "mcp_auth",
        };
        let by_guardrail = match guardrail_type {
            "pipeline_static" => action == "continue",
            "pipeline_hitl" => matches!(action, "approve" | "reject" | "edit"),
            _ => true,
        };
        guardrail && by_guardrail && self.allows(action)
    }

    /// Actions the schema allows for the kind.
    fn allows(self, action: &str) -> bool {
        match self {
            Self::ToolGuard => {
                matches!(action, "approve" | "reject" | "block_with_comment" | "edit")
            }
            Self::HitlNode => matches!(action, "approve" | "reject" | "edit" | "continue"),
            Self::AskUser => matches!(action, "answer"),
            Self::DelegatedAuth => matches!(action, "authorize" | "skip"),
        }
    }
}

/// What a child's proven pause contributes to its card: the kind, the
/// actions it offers, the closed display and the digest of the Worker's full
/// private pending object (§4).
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FanoutCardParts {
    pub(crate) kind: CardKind,
    pub(crate) available_actions: Vec<&'static str>,
    pub(crate) display: CardDisplay,
    pub(crate) payload_sha256: String,
}

/// The contract's action vocabulary (§1), as the card's static strings.
pub(crate) fn card_action(action: &str) -> Option<&'static str> {
    [
        "approve",
        "reject",
        "edit",
        "block_with_comment",
        "continue",
        "answer",
        "authorize",
        "skip",
    ]
    .into_iter()
    .find(|known| *known == action)
}

/// One card (`fanout-interrupt-card`), fields in key order.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct InterruptCardV1 {
    pub(crate) available_actions: Vec<&'static str>,
    pub(crate) display: CardDisplay,
    pub(crate) fanout_v1: Option<FanoutMemberV1>,
    pub(crate) interrupt_id: String,
    pub(crate) interrupt_key: String,
    pub(crate) kind: &'static str,
    pub(crate) parent_agent_call_id: Option<String>,
    pub(crate) parent_agent_name: Option<String>,
    pub(crate) parent_agent_path: Vec<AgentPathTier>,
    pub(crate) payload_sha256: String,
    pub(crate) schema: &'static str,
}

/// Why a frame could not be built. Names no value.
#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub(crate) enum FanoutFrameError {
    #[error("graph.fanout.invalid_card")]
    InvalidCard,
    #[error("graph.fanout.invalid_member")]
    InvalidMember,
}

pub(crate) struct CardInput<'a> {
    pub(crate) interrupt_key: &'a str,
    pub(crate) interrupt_id: &'a str,
    pub(crate) kind: CardKind,
    pub(crate) available_actions: &'a [&'static str],
    /// SHA-256 of the Worker's full private pending object.
    pub(crate) payload_sha256: &'a str,
    pub(crate) display: CardDisplay,
    pub(crate) hierarchy: &'a MemberHierarchy,
    pub(crate) member: Option<&'a FanoutMemberV1>,
}

impl InterruptCardV1 {
    pub(crate) fn new(input: CardInput<'_>) -> Result<Self, FanoutFrameError> {
        let actions_ok = (1..=4).contains(&input.available_actions.len())
            && input
                .available_actions
                .iter()
                .all(|action| input.kind.coherent(input.display.guardrail_type, action))
            && input
                .available_actions
                .iter()
                .enumerate()
                .all(|(index, action)| !input.available_actions[..index].contains(action));
        if !nonzero_hex64(input.interrupt_key)
            || !nonzero_hex64(input.payload_sha256)
            || !printable_id(input.interrupt_id)
            || !actions_ok
            || input.display.message.is_empty()
        {
            return Err(FanoutFrameError::InvalidCard);
        }
        check_hierarchy(input.hierarchy, input.member)?;
        let leaf = input.hierarchy.leaf();
        Ok(Self {
            available_actions: input.available_actions.to_vec(),
            display: input.display,
            fanout_v1: input.member.cloned(),
            interrupt_id: input.interrupt_id.to_owned(),
            interrupt_key: input.interrupt_key.to_owned(),
            kind: input.kind.tag(),
            parent_agent_call_id: leaf.map(|tier| tier.call_id.clone()),
            parent_agent_name: leaf.map(|tier| tier.name.clone()),
            parent_agent_path: input.hierarchy.path.clone(),
            payload_sha256: input.payload_sha256.to_owned(),
            schema: CARD_SCHEMA,
        })
    }
}

/// Lifecycle status of one member (`fanout-member-status`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum MemberLifecycle {
    Start { open_interrupts: usize },
    End,
    Paused { open_interrupts: usize },
    Failed,
    Cancelled,
}

#[derive(Serialize)]
struct MemberStatusV1<'a> {
    fanout_v1: &'a FanoutMemberV1,
    open_interrupts: usize,
    parent_agent_call_id: &'a str,
    parent_agent_name: &'a str,
    parent_agent_path: &'a [AgentPathTier],
    schema: &'static str,
    status: &'static str,
}

fn check_hierarchy(
    hierarchy: &MemberHierarchy,
    member: Option<&FanoutMemberV1>,
) -> Result<(), FanoutFrameError> {
    if hierarchy.path.len() > elitea_agent_runtime::graph::fanout_budget::MAX_AGENT_PATH_TIERS {
        return Err(FanoutFrameError::InvalidMember);
    }
    match member {
        // A coordinator (root) card has an empty hierarchy.
        None => {
            if hierarchy.path.is_empty() {
                Ok(())
            } else {
                Err(FanoutFrameError::InvalidMember)
            }
        }
        Some(member) => {
            let ordinal_ok = (1..=member.total).contains(&member.ordinal)
                && member.total
                    <= match member.kind {
                        "parallel" => {
                            elitea_agent_runtime::graph::fanout_budget::MAX_PARALLEL_BRANCHES
                        }
                        "map" => elitea_agent_runtime::graph::fanout_budget::MAX_FANOUT_ORDINAL,
                        _ => 0,
                    };
            let tier_ok = hierarchy
                .path
                .iter()
                .any(|tier| tier.sibling_ordinal == Some(member.ordinal));
            if ordinal_ok && tier_ok && member.activation.len() == 16 {
                Ok(())
            } else {
                Err(FanoutFrameError::InvalidMember)
            }
        }
    }
}

/// The member tier's stable `call_id` (§3.3): it depends only on the frozen
/// child thread, so it survives restarts and continuations.
pub(crate) fn member_call_id(child_thread: &str) -> String {
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(MEMBER_CALL_DOMAIN);
    update_lp(&mut hash, child_thread.as_bytes());
    format!("fo1_{}", hex(hash.finish().as_ref()))
}

/// The opaque activation label (§3.4): first 64 bits of the digest.
pub(crate) fn activation_label(private_identity: &[u8]) -> String {
    let mut hash = digest::Context::new(&digest::SHA256);
    hash.update(ACTIVATION_LABEL_DOMAIN);
    update_lp(&mut hash, private_identity);
    hex(&hash.finish().as_ref()[..8])
}

fn update_lp(hash: &mut digest::Context, bytes: &[u8]) {
    let length = u32::try_from(bytes.len()).unwrap_or(u32::MAX);
    hash.update(&length.to_be_bytes());
    hash.update(bytes);
}

/// The non-terminal frame that raises one card.
pub(crate) fn interrupt_pending_event(
    card: &InterruptCardV1,
) -> Result<NodeEventV1, FanoutFrameError> {
    // Every per-card frame says more cards may follow (v1.1 §9.2).
    let metadata = serde_json::json!({
        "fanout_v1": card.fanout_v1,
        "interrupt_card_v1": card,
        "pause_progress": true,
        "parent_agent_call_id": card.parent_agent_call_id,
        "parent_agent_name": card.parent_agent_name,
        "parent_agent_path": card.parent_agent_path,
    });
    Ok(NodeEventV1 {
        r#type: INTERRUPT_PENDING_EVENT.to_owned(),
        response_metadata: serde_json::to_vec(&metadata)
            .map_err(|_| FanoutFrameError::InvalidCard)?,
        ..NodeEventV1::default()
    })
}

/// The non-terminal member lifecycle frame.
pub(crate) fn member_status_event(
    member: &FanoutMemberV1,
    hierarchy: &MemberHierarchy,
    lifecycle: MemberLifecycle,
) -> Result<NodeEventV1, FanoutFrameError> {
    check_hierarchy(hierarchy, Some(member))?;
    let leaf = hierarchy.leaf().ok_or(FanoutFrameError::InvalidMember)?;
    let (status, open_interrupts) = match lifecycle {
        MemberLifecycle::Start { open_interrupts } => ("start", open_interrupts),
        MemberLifecycle::End => ("end", 0),
        MemberLifecycle::Paused { open_interrupts } if open_interrupts >= 1 => {
            ("paused", open_interrupts)
        }
        MemberLifecycle::Paused { .. } => return Err(FanoutFrameError::InvalidMember),
        MemberLifecycle::Failed => ("failed", 0),
        MemberLifecycle::Cancelled => ("cancelled", 0),
    };
    let body = MemberStatusV1 {
        fanout_v1: member,
        open_interrupts,
        parent_agent_call_id: &leaf.call_id,
        parent_agent_name: &leaf.name,
        parent_agent_path: &hierarchy.path,
        schema: MEMBER_STATUS_SCHEMA,
        status,
    };
    let metadata = serde_json::json!({
        "fanout_member_status_v1": body,
        "fanout_v1": member,
        "parent_agent_call_id": leaf.call_id,
        "parent_agent_name": leaf.name,
        "parent_agent_path": hierarchy.path,
    });
    Ok(NodeEventV1 {
        r#type: FANOUT_MEMBER_EVENT.to_owned(),
        response_metadata: serde_json::to_vec(&metadata)
            .map_err(|_| FanoutFrameError::InvalidMember)?,
        ..NodeEventV1::default()
    })
}

/// The activation scope a progress frame shares (`fanout_v1` without member).
#[cfg_attr(
    not(test),
    allow(
        dead_code,
        reason = "coalesced progress frames are wired with Map (2a-3)"
    )
)]
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub(crate) struct ProgressScope {
    pub(crate) activation: String,
    pub(crate) kind: &'static str,
    pub(crate) node: String,
    pub(crate) total: usize,
}

#[cfg_attr(
    not(test),
    allow(
        dead_code,
        reason = "coalesced progress frames are wired with Map (2a-3)"
    )
)]
#[derive(Serialize)]
struct ProgressMember<'a> {
    call_id: &'a str,
    ordinal: usize,
    text: &'a str,
}

#[cfg_attr(
    not(test),
    allow(
        dead_code,
        reason = "coalesced progress frames are wired with Map (2a-3)"
    )
)]
#[derive(Serialize)]
struct ProgressV1<'a> {
    fanout_v1: &'a ProgressScope,
    members: Vec<ProgressMember<'a>>,
    schema: &'static str,
}

/// One coalescer flush as `agent_fanout_progress` frames (v1.1 §9.1): members
/// in ascending ordinal, each frame at most [`MAX_PROGRESS_TEXT_BYTES`] of text
/// in total; a long member text continues, split on a character boundary, in
/// the next frame. Lossless: concatenating a member's texts in frame order
/// gives its input text.
#[cfg_attr(
    not(test),
    allow(
        dead_code,
        reason = "coalesced progress frames are wired with Map (2a-3)"
    )
)]
pub(crate) fn progress_events(
    scope: &ProgressScope,
    deltas: &[(usize, String, String)],
) -> Result<Vec<NodeEventV1>, FanoutFrameError> {
    let limit = match scope.kind {
        "parallel" => elitea_agent_runtime::graph::fanout_budget::MAX_PARALLEL_BRANCHES,
        "map" => elitea_agent_runtime::graph::fanout_budget::MAX_FANOUT_ORDINAL,
        _ => 0,
    };
    if scope.total == 0 || scope.total > limit || scope.activation.len() != 16 {
        return Err(FanoutFrameError::InvalidMember);
    }
    let mut previous = 0;
    for (ordinal, call_id, _) in deltas {
        if *ordinal <= previous
            || *ordinal > scope.total
            || call_id.len() != 68
            || !call_id.starts_with("fo1_")
        {
            return Err(FanoutFrameError::InvalidMember);
        }
        previous = *ordinal;
    }
    let mut frames: Vec<Vec<ProgressMember<'_>>> = Vec::new();
    let mut current: Vec<ProgressMember<'_>> = Vec::new();
    let mut budget = MAX_PROGRESS_TEXT_BYTES;
    for (ordinal, call_id, text) in deltas {
        let mut rest = text.as_str();
        while !rest.is_empty() {
            let mut take = rest.len().min(budget);
            while !rest.is_char_boundary(take) {
                take -= 1;
            }
            if take == 0 {
                // The frame is full, or the next character does not fit it.
                frames.push(std::mem::take(&mut current));
                budget = MAX_PROGRESS_TEXT_BYTES;
                continue;
            }
            let (head, tail) = rest.split_at(take);
            current.push(ProgressMember {
                call_id,
                ordinal: *ordinal,
                text: head,
            });
            budget -= take;
            rest = tail;
        }
    }
    frames.push(current);
    frames
        .into_iter()
        .filter(|members| !members.is_empty())
        .map(|members| {
            let body = ProgressV1 {
                fanout_v1: scope,
                members,
                schema: PROGRESS_SCHEMA,
            };
            Ok(NodeEventV1 {
                r#type: FANOUT_PROGRESS_EVENT.to_owned(),
                response_metadata: serde_json::to_vec(
                    &serde_json::json!({ "fanout_progress_v1": body }),
                )
                .map_err(|_| FanoutFrameError::InvalidMember)?,
                ..NodeEventV1::default()
            })
        })
        .collect()
}

/// The internal event that carries `frame` through the run's event stream:
/// persisted with the run, projected once, in order.
pub(crate) fn frame_event(
    invocation_id: &str,
    frame: &NodeEventV1,
) -> Result<adk_rust::Event, FanoutFrameError> {
    let metadata: serde_json::Value = serde_json::from_slice(&frame.response_metadata)
        .map_err(|_| FanoutFrameError::InvalidMember)?;
    let mut event = adk_rust::Event::new(invocation_id);
    FANOUT_FRAME_AUTHOR.clone_into(&mut event.author);
    event.provider_metadata.insert(
        FANOUT_FRAME_METADATA_KEY.to_owned(),
        serde_json::json!({"type": frame.r#type, "metadata": metadata}).to_string(),
    );
    Ok(event)
}

/// The frame a stamped event carries: its type and metadata. `None` for any
/// other event; a stamp that is not exactly a fan-out frame is refused.
pub(crate) fn stamped_frame(
    event: &adk_rust::Event,
) -> Result<Option<(&'static str, serde_json::Value)>, FanoutFrameError> {
    #[derive(serde::Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Stamp {
        r#type: String,
        metadata: serde_json::Map<String, serde_json::Value>,
    }
    let Some(raw) = event.provider_metadata.get(FANOUT_FRAME_METADATA_KEY) else {
        return Ok(None);
    };
    let stamp: Stamp = serde_json::from_str(raw).map_err(|_| FanoutFrameError::InvalidMember)?;
    let frame_type = [
        INTERRUPT_PENDING_EVENT,
        FANOUT_MEMBER_EVENT,
        FANOUT_PROGRESS_EVENT,
    ]
    .into_iter()
    .find(|known| *known == stamp.r#type)
    .ok_or(FanoutFrameError::InvalidMember)?;
    if event.author != FANOUT_FRAME_AUTHOR
        || event.content().is_some()
        || raw.len() > MAX_FRAME_METADATA_BYTES
    {
        return Err(FanoutFrameError::InvalidMember);
    }
    Ok(Some((
        frame_type,
        serde_json::Value::Object(stamp.metadata),
    )))
}

fn nonzero_hex64(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        && value.bytes().any(|byte| byte != b'0')
}

fn printable_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 512
        && value.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
        && !value.starts_with(' ')
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut out, byte| {
            let _ = write!(out, "{byte:02x}");
            out
        })
}

#[cfg(test)]
#[path = "fanout_frames_tests.rs"]
mod tests;
