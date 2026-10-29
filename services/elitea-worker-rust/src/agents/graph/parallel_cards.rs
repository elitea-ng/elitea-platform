//! Per-interrupt cards of a fan-out child that owns a saved pipeline
//! (2a-2 task 7; contract §4, design §5.1).
//!
//! A child's pause lives on its own checkpoint, so the card is proven against
//! that checkpoint, never against the session tail or the root thread's
//! naming. The child's pause is replayed as the exact interrupt event the
//! root graph agent would publish for it, under a fan-out author and an
//! invocation id derived from the frozen child thread, and fed to the same
//! typed bindings and resolvers the root path uses. The interrupt id is
//! therefore a pure function of `(child thread, the child's paused checkpoint,
//! the pause data)` and survives restarts and continuations.
//!
//! Kinds: `hitl_node` (a HITL node, or an authored static pause continued with
//! text), `tool_guard`, `ask_user` and `delegated_auth`. A saved Agent child
//! raises its cards through its own nested Application pause; those decisions
//! need the Application decision registry, so its pause fails typed here.

use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
use adk_rust::graph::{GraphError, State};
use adk_rust::{Content, Event};
use elitea_agent_runtime::graph::fanout::identity::member_call_id;

use super::parallel::{ParallelBranchPause, ParallelDecision, ParallelPauseCard};
use super::resume::{
    PipelineHitlDecision, PipelineMcpAuthorizationContinuation, PipelineResumeError,
    PipelineToolDecision,
};
use super::static_pause::{StaticPauseCatalog, fanout_static_pause_id, fanout_static_resume};
use crate::agents::events::{
    pipeline_clarifying_event_binding, pipeline_hitl_event_binding,
    pipeline_mcp_auth_event_binding, pipeline_tool_event_binding,
};

/// The author of a replayed child pause; no session ever holds it.
const FANOUT_CHILD_AUTHOR: &str = "elitea-fanout-child";

/// The card kinds a fan-out child raises (contract §4 `kind`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FanoutCardKind {
    ToolGuard,
    HitlNode,
    /// An authored `interrupt_before`/`interrupt_after` pause: `continue` only.
    StaticPause,
    AskUser,
    DelegatedAuth,
}

impl FanoutCardKind {
    /// The decision actions this kind accepts (contract §4).
    #[must_use]
    pub(crate) fn accepts(self, action: &str) -> bool {
        match self {
            Self::ToolGuard => matches!(action, "approve" | "reject" | "block_with_comment"),
            Self::HitlNode => {
                matches!(action, "approve" | "reject" | "edit" | "block_with_comment")
            }
            Self::StaticPause => action == "continue",
            Self::AskUser => action == "answer",
            Self::DelegatedAuth => matches!(action, "authorize" | "skip"),
        }
    }
}

/// One proven card of a child pause.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct FanoutCard {
    pub(crate) kind: FanoutCardKind,
    pub(crate) card: ParallelPauseCard,
}

/// The child's pause as the root graph agent would publish it.
fn pause_event(pause: &ParallelBranchPause) -> Event {
    let mut event = Event::new(format!("fanout:{}", member_call_id(&pause.thread_id)));
    FANOUT_CHILD_AUTHOR.clone_into(&mut event.author);
    event.set_content(Content::new("assistant").with_text(pause.interrupt.to_string()));
    event.provider_metadata.insert(
        INTERRUPT_METADATA_KEY.to_owned(),
        GraphInterruptPayload::new(&pause.interrupt, &pause.thread_id, &pause.checkpoint_id)
            .to_metadata_value(),
    );
    event
}

fn card(kind: FanoutCardKind, interrupt_id: &str, tool_call_id: &str) -> FanoutCard {
    FanoutCard {
        kind,
        card: ParallelPauseCard {
            interrupt_id: interrupt_id.to_owned(),
            tool_call_id: tool_call_id.to_owned(),
        },
    }
}

/// FU-246: the ledger card a saved Agent member's own pause carries (its
/// Agent node's interrupt data, `fanout_cards`), proven against that pause's
/// own interrupt ids. `None` for any other pause.
fn application_member_card(
    pause: &ParallelBranchPause,
) -> Option<(
    FanoutCardKind,
    ParallelPauseCard,
    crate::agents::graph::fanout_frames::FanoutCardParts,
)> {
    let adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(data), ..
    } = &pause.interrupt
    else {
        return None;
    };
    if data
        .get("schema_revision")
        .and_then(serde_json::Value::as_str)
        != Some(crate::agents::graph::PIPELINE_APPLICATION_HITL_SCHEMA)
    {
        return None;
    }
    let [card] = data.get("fanout_cards")?.as_array()?.as_slice() else {
        return None;
    };
    let (kind, interrupt_id, tool_call_id, parts) =
        crate::agents::events::fanout_member_card_parts(card)?;
    let ids = data.get("interrupt_ids")?.as_array()?;
    if ids.len() != 1 || ids[0].as_str() != Some(interrupt_id.as_str()) {
        return None;
    }
    let kind = match kind {
        crate::agents::graph::fanout_frames::CardKind::ToolGuard => FanoutCardKind::ToolGuard,
        crate::agents::graph::fanout_frames::CardKind::AskUser => FanoutCardKind::AskUser,
        _ => return None,
    };
    Some((
        kind,
        ParallelPauseCard {
            interrupt_id,
            tool_call_id,
        },
        parts,
    ))
}

/// Whether the child's pause is a saved Agent member's own carded pause,
/// resumed through the Agent's nested decision registry, not the bindings here.
pub(crate) fn is_application_member_pause(pause: &ParallelBranchPause) -> bool {
    application_member_card(pause).is_some()
}

/// Classify and bind the child's pause. Exactly one typed binding accepts
/// a given pause shape; anything else is refused typed.
pub(crate) fn child_card(pause: &ParallelBranchPause) -> Result<FanoutCard, GraphError> {
    if let Some((kind, card, _)) = application_member_card(pause) {
        return Ok(FanoutCard { kind, card });
    }
    let event = pause_event(pause);
    let thread = pause.thread_id.as_str();
    if let Ok(binding) = pipeline_hitl_event_binding(&event, FANOUT_CHILD_AUTHOR, thread) {
        return Ok(card(FanoutCardKind::HitlNode, binding.interrupt_id(), ""));
    }
    if let Ok(binding) = pipeline_tool_event_binding(&event, FANOUT_CHILD_AUTHOR, thread) {
        return Ok(card(
            FanoutCardKind::ToolGuard,
            binding.interrupt_id(),
            binding.tool_call_id(),
        ));
    }
    if let Ok(binding) = pipeline_clarifying_event_binding(&event, FANOUT_CHILD_AUTHOR, thread) {
        return Ok(card(
            FanoutCardKind::AskUser,
            binding.interrupt_id(),
            binding.tool_call_id(),
        ));
    }
    if let Ok(binding) = pipeline_mcp_auth_event_binding(&event, FANOUT_CHILD_AUTHOR, thread) {
        return Ok(card(
            FanoutCardKind::DelegatedAuth,
            binding.interrupt_id(),
            binding.tool_call_id(),
        ));
    }
    if let Some(pause_id) = fanout_static_pause_id(&event, FANOUT_CHILD_AUTHOR, thread) {
        return Ok(card(FanoutCardKind::StaticPause, &pause_id, ""));
    }
    Err(fanout_card_error(
        "graph.parallel.pause_unproved",
        "the branch pause has no child checkpoint proof",
    ))
}

/// The card frame parts of the child's pause: its kind, actions, display and
/// the digest of its private pending object (contract §4).
pub(crate) fn card_parts(
    pause: &ParallelBranchPause,
) -> Result<crate::agents::graph::fanout_frames::FanoutCardParts, GraphError> {
    if let Some((_, _, parts)) = application_member_card(pause) {
        return Ok(parts);
    }
    let event = pause_event(pause);
    crate::agents::events::fanout_card_parts(&event, FANOUT_CHILD_AUTHOR, &pause.thread_id).map_err(
        |_| {
            fanout_card_error(
                "graph.parallel.pause_unproved",
                "the branch pause cannot be shown as a card",
            )
        },
    )
}

/// Resume controls for exactly this child's decision, proven against the
/// child's own checkpoints. `catalog` is the child participant's authored
/// pause catalog (static pauses only).
pub(crate) async fn child_resume(
    pause: &ParallelBranchPause,
    catalog: Option<&StaticPauseCatalog>,
    decisions: &[ParallelDecision],
) -> Result<State, GraphError> {
    let [decision] = decisions else {
        // One child pause holds one card: one resume carries one decision.
        return Err(fanout_card_error(
            "graph.parallel.invalid_decision",
            "a pipeline child pause takes exactly one decision",
        ));
    };
    let proven = child_card(pause)?;
    if decision.interrupt_id != proven.card.interrupt_id
        || decision.tool_call_id != proven.card.tool_call_id
    {
        return Err(fanout_card_error(
            "graph.parallel.stale_decision",
            "the decision does not answer the child's current pause",
        ));
    }
    if !proven.kind.accepts(&decision.action) {
        return Err(fanout_card_error(
            "graph.parallel.invalid_decision",
            "the decision action is not offered by this card",
        ));
    }
    let event = pause_event(pause);
    let checkpointer = pause.checkpointer.as_ref();
    let thread = pause.thread_id.as_str();
    let id = proven.card.interrupt_id.as_str();
    let call = proven.card.tool_call_id.as_str();
    let resumed = match proven.kind {
        FanoutCardKind::HitlNode => {
            PipelineHitlDecision::fanout(id, &decision.action, &decision.value)
                .map_err(resume_error)?
                .resolve_event(&event, checkpointer, FANOUT_CHILD_AUTHOR, thread)
                .await
        }
        FanoutCardKind::ToolGuard | FanoutCardKind::AskUser => {
            PipelineToolDecision::fanout(id, call, &decision.action, &decision.value)
                .map_err(resume_error)?
                .resolve_event(&event, checkpointer, FANOUT_CHILD_AUTHOR, thread)
                .await
        }
        FanoutCardKind::DelegatedAuth => {
            PipelineMcpAuthorizationContinuation::fanout(id, call, &decision.action)
                .map_err(resume_error)?
                .resolve_event(&event, checkpointer, FANOUT_CHILD_AUTHOR, thread)
                .await
        }
        FanoutCardKind::StaticPause => {
            let catalog = catalog.ok_or_else(|| {
                fanout_card_error(
                    "graph.parallel.pause_unproved",
                    "the branch has no authored pause catalog",
                )
            })?;
            fanout_static_resume(
                &event,
                checkpointer,
                FANOUT_CHILD_AUTHOR,
                thread,
                catalog,
                id,
                &decision.value,
            )
            .await
        }
    };
    Ok(resumed.map_err(resume_error)?.into_state())
}

#[allow(clippy::needless_pass_by_value)] // Shaped for `map_err`.
fn resume_error(error: PipelineResumeError) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "parallel".to_owned(),
        message: format!(
            "graph.parallel.decision_refused: the child pause refused the decision ({})",
            error.code().as_str()
        ),
    }
}

fn fanout_card_error(code: &str, message: &str) -> GraphError {
    GraphError::NodeExecutionFailed {
        node: "parallel".to_owned(),
        message: format!("{code}: {message}"),
    }
}
