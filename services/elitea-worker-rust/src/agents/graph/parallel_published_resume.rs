//! Bind aggregate decisions to the exact latest published parent pause.

use adk_rust::Event;
use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
use adk_rust::graph::{Checkpoint, Checkpointer, ExecutionConfig, GraphError, NodeContext, State};
use adk_rust::session::Session;
use serde_json::json;

use super::checkpoint::occurrence_from;
use super::{
    PARALLEL_INTERRUPT_SCHEMA, PARALLEL_RESUME_STATE_KEY, ParallelActivation, ParallelDecision,
    ParallelOccurrenceCheckpointer, activation_label, decisions_for_activation, parallel_error,
};
use crate::agents::graph::resume::PipelineResume;

pub(crate) struct ParallelPublishedContinuation;

impl ParallelPublishedContinuation {
    /// This proves the aggregate parent. The branch factory separately proves
    /// every descendant card and resume input before any accepted decision runs.
    pub(crate) async fn resolve(
        session: &dyn Session,
        checkpointer: &ParallelOccurrenceCheckpointer,
        root_agent: &str,
        activation: &ParallelActivation,
        decisions: &[ParallelDecision],
    ) -> Result<PipelineResume, GraphError> {
        let events = session.events().all();
        let event = events.last().ok_or_else(stale)?;
        let payload = validated_event_pause(event)?;
        let checkpoint = checkpointer
            .load_by_id(&payload.checkpoint_id)
            .await?
            .ok_or_else(stale)?;
        let latest = checkpointer
            .load(&activation.root_thread_id)
            .await?
            .ok_or_else(stale)?;
        super::structure::validate_checkpoint(&checkpoint)?;
        super::structure::validate_checkpoint(&latest)?;
        // An old immutable checkpoint does not authorize the current frontier.
        if serde_json::to_value(&checkpoint).map_err(|_| stale())?
            != serde_json::to_value(&latest).map_err(|_| stale())?
        {
            return Err(stale());
        }
        let state =
            validate_published_parent(event, &checkpoint, root_agent, activation, decisions)?;
        Ok(PipelineResume::from_state(state))
    }
}

fn validate_published_parent(
    event: &Event,
    checkpoint: &Checkpoint,
    root_agent: &str,
    activation: &ParallelActivation,
    decisions: &[ParallelDecision],
) -> Result<State, GraphError> {
    super::structure::validate_checkpoint(checkpoint)?;
    let payload = validated_event_pause(event)?;
    if event.author != root_agent
        || root_agent.is_empty()
        || event.llm_response.partial
        || event.actions.tool_confirmation.is_some()
        || payload.kind != "dynamic"
        || payload.node.is_some()
        || payload.thread_id != activation.root_thread_id
        || payload.checkpoint_id != checkpoint.checkpoint_id
        || checkpoint.cleared_interrupt.is_some()
    {
        return Err(stale());
    }
    let occurrence = occurrence_from(checkpoint, activation)?;
    if occurrence.cards.is_empty() || occurrence.decisions.is_some() || occurrence.blocked.is_some()
    {
        return Err(stale());
    }
    let mut cards = Vec::with_capacity(occurrence.cards.len());
    for (ordinal, card) in &occurrence.cards {
        let branch = occurrence
            .branches
            .get(*ordinal)
            .filter(|branch| branch.ordinal == *ordinal)
            .ok_or_else(stale)?;
        cards.push(json!({
            "branch_id": branch.branch_id, "node": branch.node, "ordinal": ordinal,
            "interrupt_id": card.interrupt_id, "tool_call_id": card.tool_call_id,
        }));
    }
    let aggregate = json!({
        "schema": PARALLEL_INTERRUPT_SCHEMA, "parallel_node": activation.node_id,
        "parallel_activation": activation_label(activation)?, "cards": cards,
    });
    if payload.data.as_ref() != Some(&aggregate) {
        return Err(stale());
    }
    let state = State::from([(
        PARALLEL_RESUME_STATE_KEY.to_owned(),
        json!({
            "schema": PARALLEL_INTERRUPT_SCHEMA,
            "parallel_activation": activation_label(activation)?, "decisions": decisions,
        }),
    )]);
    let context = NodeContext::new(
        state.clone(),
        ExecutionConfig::new(&activation.root_thread_id),
        checkpoint.step,
    );
    if decisions_for_activation(&context, activation, &occurrence.cards)?.is_none() {
        return Err(stale());
    }
    Ok(state)
}

fn validated_event_pause(event: &Event) -> Result<GraphInterruptPayload, GraphError> {
    let raw = event
        .provider_metadata
        .get(INTERRUPT_METADATA_KEY)
        .ok_or_else(stale)?;
    if raw.len() > super::MAX_PAUSE_BYTES {
        return Err(stale());
    }
    // Event metadata is JSON text. serde's parser owns its recursion bound;
    // validate the parsed typed data before comparisons or further cloning.
    let payload = GraphInterruptPayload::from_event(event).ok_or_else(stale)?;
    super::validate_values(payload.data.iter())?;
    Ok(payload)
}

fn stale() -> GraphError {
    parallel_error(
        "graph.parallel.stale_published_pause",
        "the exact current published parent pause was not proved",
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::agents::graph::parallel::ParallelPauseCard;
    use crate::agents::graph::parallel::checkpoint::{
        FrozenBranchInput, FrozenOccurrence, OCCURRENCE_KEY,
    };
    use adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY;
    use std::collections::BTreeMap;

    fn fixture() -> (Event, Checkpoint, ParallelActivation, Vec<ParallelDecision>) {
        let activation = ParallelActivation {
            root_thread_id: "parent".into(),
            node_id: "gather".into(),
            step: 4,
            config_digest: [3; 32],
        };
        let branches = (0..2)
            .map(|ordinal| FrozenBranchInput {
                branch_id: format!("branch{ordinal}"),
                node: format!("owned{ordinal}"),
                ordinal,
                owned_definition_digest: [1; 32],
                input_digest: [2; 32],
                input: State::new(),
            })
            .collect::<Vec<_>>();
        let cards = vec![(
            1,
            ParallelPauseCard {
                interrupt_id: "card".into(),
                tool_call_id: "call".into(),
            },
        )];
        let occurrence = FrozenOccurrence {
            activation: activation.clone(),
            branches,
            origin: crate::agents::graph::ParallelChildOrigin {
                execution_id: "execution".into(),
                generation: 1,
            },
            child_threads: vec!["child0".into(), "child1".into()],
            cards,
            decisions: None,
            resume_inputs: BTreeMap::new(),
            blocked: None,
        };
        let mut checkpoint = Checkpoint::new("parent", State::new(), 4, vec!["gather".into()]);
        checkpoint.metadata.insert(
            OCCURRENCE_KEY.into(),
            serde_json::to_value(occurrence).unwrap(),
        );
        let aggregate = json!({"schema": PARALLEL_INTERRUPT_SCHEMA, "parallel_node":"gather", "parallel_activation":activation_label(&activation).unwrap(), "cards":[{"branch_id":"branch1","node":"owned1","ordinal":1,"interrupt_id":"card","tool_call_id":"call"}]});
        let payload = GraphInterruptPayload {
            kind: "dynamic".into(),
            node: None,
            message: Some("Parallel branches paused.".into()),
            data: Some(aggregate),
            thread_id: "parent".into(),
            checkpoint_id: checkpoint.checkpoint_id.clone(),
        };
        let mut event = Event::new("invocation");
        event.author = "root".into();
        event
            .provider_metadata
            .insert(INTERRUPT_METADATA_KEY.into(), payload.to_metadata_value());
        let decisions = vec![ParallelDecision {
            interrupt_id: "card".into(),
            tool_call_id: "call".into(),
            action: "approve".into(),
            value: String::new(),
        }];
        (event, checkpoint, activation, decisions)
    }

    #[test]
    fn exact_published_parent_accepts_only_the_full_bound_card_set() {
        let (event, checkpoint, activation, decisions) = fixture();
        let state = validate_published_parent(&event, &checkpoint, "root", &activation, &decisions)
            .unwrap();
        assert_eq!(state.len(), 1);
        assert!(validate_published_parent(&event, &checkpoint, "root", &activation, &[]).is_err());
        let mut foreign = decisions;
        foreign[0].tool_call_id = "foreign".into();
        assert!(
            validate_published_parent(&event, &checkpoint, "root", &activation, &foreign).is_err()
        );
    }

    #[test]
    fn forged_author_frontier_activation_and_aggregate_are_refused() {
        let (event, checkpoint, activation, decisions) = fixture();
        for variation in 0..6 {
            let mut event = event.clone();
            let mut checkpoint = checkpoint.clone();
            let mut activation = activation.clone();
            match variation {
                0 => event.author = "other".into(),
                1 => checkpoint.pending_nodes = vec!["after".into()],
                2 => activation.config_digest = [7; 32],
                3 => checkpoint.cleared_interrupt = Some("gather".into()),
                4 => {
                    let mut payload = GraphInterruptPayload::from_event(&event).unwrap();
                    payload.checkpoint_id = "old".into();
                    event
                        .provider_metadata
                        .insert(INTERRUPT_METADATA_KEY.into(), payload.to_metadata_value());
                }
                5 => {
                    let mut payload = GraphInterruptPayload::from_event(&event).unwrap();
                    payload.data.as_mut().unwrap()["cards"][0]["branch_id"] = json!("other");
                    event
                        .provider_metadata
                        .insert(INTERRUPT_METADATA_KEY.into(), payload.to_metadata_value());
                }
                _ => unreachable!(),
            }
            assert!(
                validate_published_parent(&event, &checkpoint, "root", &activation, &decisions)
                    .is_err()
            );
        }
    }
}
