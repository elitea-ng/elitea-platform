//! The Parallel node's card and member frames (contract §3.3, §3.4, §4, §9),
//! built per activation and sent through the run's event stream in order:
//! Main raises a card only from its frame, so a frame that cannot be sent
//! stops the run instead of leaving a card nobody can decide.

use std::sync::Arc;

use async_trait::async_trait;
use elitea_agent_runtime::graph::fanout::apply::CardIdentity;
use elitea_agent_runtime::graph::fanout::runner::{FanoutEvents, FanoutRunError, MemberStatus};

use super::{CardBoard, ParallelActivation};
use crate::agents::graph::fanout_frames::{
    AgentPathTier, CardInput, FanoutMemberV1, InterruptCardV1, MemberHierarchy, MemberLifecycle,
    activation_label, frame_event, interrupt_pending_event, member_call_id, member_status_event,
};
use crate::agents::graph::node_events::{PipelineNodeEventScope, PipelineNodeEventSender};

/// One member's identity: its `fanout_v1` and its full hierarchy.
struct Member {
    fanout: FanoutMemberV1,
    hierarchy: MemberHierarchy,
}

/// A member as the node knows it: branch id, display name, frozen thread.
pub(super) struct MemberInput<'a> {
    pub(super) branch_id: &'a str,
    pub(super) name: &'a str,
    pub(super) child_thread: &'a str,
}

pub(crate) struct ParallelFrames {
    sender: PipelineNodeEventSender,
    invocation_id: String,
    board: Arc<CardBoard>,
    members: Vec<Member>,
}

impl ParallelFrames {
    /// `enclosing` is the node's own event scope: the tiers of the agents
    /// that own this pipeline come before the member tier (§3.3).
    pub(super) fn new(
        sender: PipelineNodeEventSender,
        invocation_id: &str,
        board: Arc<CardBoard>,
        activation: &ParallelActivation,
        members: &[MemberInput<'_>],
        enclosing: Option<&PipelineNodeEventScope>,
    ) -> Result<Self, FanoutRunError> {
        let identity = serde_json::to_vec(&(
            &activation.root_thread_id,
            &activation.node_id,
            activation.step,
            activation.config_digest,
        ))
        .map_err(|_| FanoutRunError::InvalidConfiguration)?;
        let label = activation_label(&identity);
        let mut outer = Vec::new();
        let mut scope = enclosing;
        while let Some(current) = scope {
            outer.push(AgentPathTier {
                call_id: current.parent_call_id().to_owned(),
                name: current.agent_name().to_owned(),
                sibling_ordinal: None,
            });
            scope = current.parent();
        }
        outer.reverse();
        let total = members.len();
        let members = members
            .iter()
            .enumerate()
            .map(|(index, member)| {
                let mut path = outer.clone();
                path.push(AgentPathTier {
                    call_id: member_call_id(member.child_thread),
                    name: member.name.to_owned(),
                    sibling_ordinal: Some(index + 1),
                });
                Member {
                    fanout: FanoutMemberV1 {
                        activation: label.clone(),
                        kind: "parallel",
                        member: member.branch_id.to_owned(),
                        node: activation.node_id.clone(),
                        ordinal: index + 1,
                        total,
                    },
                    hierarchy: MemberHierarchy { path },
                }
            })
            .collect();
        Ok(Self {
            sender,
            invocation_id: invocation_id.to_owned(),
            board,
            members,
        })
    }

    async fn send(
        &self,
        frame: &crate::protocol::elitea::runtime::v1::NodeEventV1,
    ) -> Result<(), FanoutRunError> {
        let event = frame_event(&self.invocation_id, frame)
            .map_err(|_| FanoutRunError::FrameUndelivered)?;
        self.sender
            .send_routed_application_event(event)
            .await
            .map_err(|_| FanoutRunError::FrameUndelivered)
    }
}

#[async_trait]
impl FanoutEvents for ParallelFrames {
    async fn card_opened(&self, child: usize, card: &CardIdentity) -> Result<(), FanoutRunError> {
        let member = self
            .members
            .get(child)
            .ok_or(FanoutRunError::CorruptChild)?;
        let parts = self
            .board
            .take(&card.interrupt_key)
            .ok_or(FanoutRunError::FrameUndelivered)?;
        let card = InterruptCardV1::new(CardInput {
            interrupt_key: &card.interrupt_key,
            interrupt_id: &card.interrupt_id,
            kind: parts.kind,
            available_actions: &parts.available_actions,
            payload_sha256: &parts.payload_sha256,
            display: parts.display,
            hierarchy: &member.hierarchy,
            member: Some(&member.fanout),
        })
        .map_err(|_| FanoutRunError::FrameUndelivered)?;
        let frame = interrupt_pending_event(&card).map_err(|_| FanoutRunError::FrameUndelivered)?;
        self.send(&frame).await
    }

    async fn member(&self, child: usize, status: MemberStatus) -> Result<(), FanoutRunError> {
        let member = self
            .members
            .get(child)
            .ok_or(FanoutRunError::CorruptChild)?;
        let lifecycle = match status {
            MemberStatus::Start => MemberLifecycle::Start { open_interrupts: 0 },
            MemberStatus::End => MemberLifecycle::End,
            MemberStatus::Paused { open_interrupts } => MemberLifecycle::Paused { open_interrupts },
            MemberStatus::Failed => MemberLifecycle::Failed,
            MemberStatus::Cancelled => MemberLifecycle::Cancelled,
        };
        let frame = member_status_event(&member.fanout, &member.hierarchy, lifecycle)
            .map_err(|_| FanoutRunError::FrameUndelivered)?;
        self.send(&frame).await
    }
}
