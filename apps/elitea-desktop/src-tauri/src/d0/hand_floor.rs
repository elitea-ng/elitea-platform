//! The `hand_floor` tool of a LOCAL agent's turn (ADR-0061 decision 3d,
//! #3064): the agent hands the floor to other agents of the thread.
//!
//! The tool's name, description, schema and argument check are the cloud
//! worker's ([`elitea_agent_runtime::hand_floor`]), so the model sees one
//! tool wherever it runs. Main decides the hand-off (`handLocalFloor`): who
//! may answer, in what order, and when a loop guard stops the exchange. The
//! host only checks the arguments against the agents Main named for the
//! turn, queues the hand-offs Main answered in the turn's exchange
//! ([`super::exchange::Exchanges::push_hands`]), and starts them after the
//! turn's commit lands (`AgentHost::continue_floor`). Every failure is a
//! tool RESULT the model reads, never a failed turn.
//!
//! Offered only to a local agent's turn of a thread with another agent
//! (never to a cloud member's turn: cloud members are leaves in v1, and the
//! cloud agent's turn has no tools), and not to an unattended run (its
//! chain continues instead). Plan mode keeps it: it changes no file.
//!
//! The tool is the turn's, never the workspace runner's: it is kept out of
//! `workspace-runner`'s `TOOLS` (its contract test documents the
//! exclusion), as the other host-only toolsets are.

use std::collections::HashSet;
use std::sync::Arc;

use adk_core::{Tool, ToolContext, Toolset};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use elitea_agent_runtime::hand_floor;
use serde_json::{Value, json};

use super::api::{FloorAgent, PlatformApi};
use super::events::TurnEvents;
use super::exchange::Exchanges;

/// The most consult pairs a hand-off sends (Main's bound).
pub const MAX_CONSULT_PAIRS: usize = 8;

/// What Main's answer says after a guard's stop text.
const GUARD_SUFFIX: &str = " Answer the person yourself.";

/// Where a hand-off goes, and what it is part of.
#[derive(Clone)]
pub struct HandFloorContext {
    pub api: Arc<PlatformApi>,
    pub project_id: i64,
    /// The handing turn's execution.
    pub execution_id: String,
    /// The person's message the exchange answers.
    pub question_id: String,
    /// The handing agent.
    pub from: i64,
    pub from_name: String,
    pub exchanges: Arc<Exchanges>,
    pub events: Arc<TurnEvents>,
}

/// The toolset for a turn of `from` among `agents`, or `None` when no other
/// agent is in the thread.
#[must_use]
pub fn toolset(context: HandFloorContext, agents: &[FloorAgent]) -> Option<Arc<dyn Toolset>> {
    let peers: Vec<hand_floor::Peer> = agents
        .iter()
        .filter(|agent| agent.participant_id != context.from && agent.participant_id > 0)
        .map(|agent| hand_floor::Peer {
            participant_id: agent.participant_id,
            name: agent.name.clone(),
            role: if agent.executor == super::api::EXECUTOR_CLOUD {
                "cloud agent".to_owned()
            } else {
                "local agent".to_owned()
            },
        })
        .collect();
    if peers.is_empty() {
        return None;
    }
    let tool: Arc<dyn Tool> = Arc::new(HandFloorTool {
        description: hand_floor::describe(&peers),
        peers: peers.iter().map(|peer| peer.participant_id).collect(),
        names: peers
            .iter()
            .map(|peer| (peer.participant_id, peer.name.clone()))
            .collect(),
        context,
    });
    Some(Arc::new(BasicToolset::new(
        hand_floor::TOOLSET_NAME,
        vec![tool],
    )))
}

struct HandFloorTool {
    description: String,
    peers: HashSet<i64>,
    names: Vec<(i64, String)>,
    context: HandFloorContext,
}

impl HandFloorTool {
    fn name_of(&self, participant_id: i64, fallback: &str) -> String {
        if !fallback.is_empty() {
            return fallback.to_owned();
        }
        self.names
            .iter()
            .find(|(id, _)| *id == participant_id)
            .map(|(_, name)| name.clone())
            .unwrap_or_default()
    }

    /// The body `handLocalFloor` gets: the checked arguments and the
    /// message's consult pairs so far (they count toward the hops).
    fn body(&self, args: &hand_floor::HandFloorArgs) -> Value {
        let mut body = json!({
            "participant_ids": args.participant_ids,
            "message": args.message,
        });
        let pairs = self.context.exchanges.consults(&self.context.question_id);
        if !pairs.is_empty() {
            let skip = pairs.len().saturating_sub(MAX_CONSULT_PAIRS);
            body["consults"] = pairs
                .iter()
                .skip(skip)
                .map(|(from, to)| json!({ "from": from, "to": to }))
                .collect();
        }
        body
    }
}

#[async_trait]
impl Tool for HandFloorTool {
    fn name(&self) -> &str {
        hand_floor::HAND_FLOOR
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(hand_floor::parameters_schema())
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        let args = match hand_floor::validate(&args, &self.peers) {
            Ok(args) => args,
            Err(result) => return Ok(result),
        };
        let context = &self.context;
        if context.exchanges.stopped(&context.question_id) {
            return Ok(hand_floor::error(
                "exchange_stopped",
                "The person stopped this exchange: nobody else answers. Finish your answer to the person.",
            ));
        }
        let (raw, answer) = match context
            .api
            .hand_local_floor(context.project_id, &context.execution_id, &self.body(&args))
            .await
        {
            Ok(answer) => answer,
            Err(error) => {
                let message = if error.message.is_empty() {
                    "The floor could not be handed.".to_owned()
                } else {
                    error.message
                };
                return Ok(hand_floor::error(&error.code, &message));
            }
        };
        // A loop guard stopped the exchange: Main skipped every queued
        // hand-off and posted the stop line; nothing more starts here.
        if !answer.stopped.is_empty() {
            let stop_text = answer
                .message
                .strip_suffix(GUARD_SUFFIX)
                .unwrap_or(&answer.message);
            context.exchanges.stop_question(&context.question_id);
            super::floor::guard_stopped(&context.events, &answer.stopped, stop_text);
            return Ok(raw);
        }
        if answer.handed_to.is_empty() {
            return Ok(raw);
        }
        if context
            .exchanges
            .push_hands(
                &context.question_id,
                context.from,
                context.project_id,
                &answer.handed_to,
            )
            .is_none()
        {
            // The person stopped the exchange while Main queued them: Main
            // records them skipped, and the model hears it.
            let hand_ids: Vec<String> = answer
                .handed_to
                .iter()
                .map(|hand| hand.hand_id.clone())
                .collect();
            skip_hands(
                context.api.clone(),
                context.project_id,
                context.question_id.clone(),
                hand_ids,
                super::exchange::STOPPED_BY_PERSON,
            );
            return Ok(hand_floor::error(
                "exchange_stopped",
                "The person stopped this exchange: nobody else answers. Finish your answer to the person.",
            ));
        }
        context.events.send(
            "floor_handed",
            json!({
                "from": context.from,
                "from_name": context.from_name,
                "to": answer
                    .handed_to
                    .iter()
                    .map(|hand| json!({
                        "participant_id": hand.participant_id,
                        "name": self.name_of(hand.participant_id, &hand.name),
                        "hand_id": hand.hand_id,
                    }))
                    .collect::<Vec<_>>(),
                "message_excerpt": super::floor::excerpt(&args.message),
            }),
        );
        Ok(raw)
    }
}

/// Tell Main the queued hand-offs `hand_ids` of `question` will not start
/// (`code`: `stopped_by_person` or `skipped_by_person`), in the background:
/// a failure only leaves them to Main's own sweep.
pub fn skip_hands(
    api: Arc<PlatformApi>,
    project_id: i64,
    question: String,
    hand_ids: Vec<String>,
    code: &'static str,
) {
    if hand_ids.is_empty() {
        return;
    }
    let Ok(runtime) = tokio::runtime::Handle::try_current() else {
        return;
    };
    runtime.spawn(async move {
        for chunk in hand_ids.chunks(16) {
            if let Err(error) = api
                .skip_local_floor_hands(project_id, &question, chunk, code)
                .await
            {
                log::warn!("queued hand-offs were not recorded skipped: {}", error.code);
            }
        }
    });
}
