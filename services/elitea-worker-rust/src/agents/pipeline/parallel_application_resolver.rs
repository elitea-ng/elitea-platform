//! Rebind only the frozen admitted participant and its existing capabilities.

use std::collections::BTreeMap;
use std::sync::Arc;

use ring::digest;

use super::{
    ApplicationExecutionError, NativePipelineApplicationParticipant,
    NativePipelineApplicationResolver, PipelineNodeEventSender,
};

impl NativePipelineApplicationResolver {
    pub(super) fn rebind_parallel_events(
        &self,
        events: &PipelineNodeEventSender,
    ) -> Result<Self, ApplicationExecutionError> {
        let nodes = self
            .nodes
            .iter()
            .map(|(node, (alias, participant))| {
                rebind_participant(participant, events)
                    .map(|participant| (node.clone(), (alias.clone(), participant)))
            })
            .collect::<Result<BTreeMap<_, _>, _>>()?;
        let participants = self
            .participants
            .iter()
            .map(|(alias, participant)| {
                rebind_participant(participant, events)
                    .map(|participant| (alias.clone(), participant))
            })
            .collect::<Result<BTreeMap<_, _>, _>>()?;
        let call_owners = self
            .call_owners
            .iter()
            .map(|(node, owner)| {
                (
                    node.clone(),
                    Arc::new(owner.with_parallel_events(events.clone())),
                )
            })
            .collect();
        Ok(Self {
            call_owners,
            nodes,
            participants,
        })
    }

    pub(super) fn parallel_participant_digest(
        &self,
        node: &str,
    ) -> Result<[u8; 32], ApplicationExecutionError> {
        if !self.call_owners.contains_key(node) {
            return Err(ApplicationExecutionError::Unavailable);
        }
        let (_, participant) = self
            .nodes
            .get(node)
            .ok_or(ApplicationExecutionError::Unavailable)?;
        let (application_id, version_id, definition_digest) = match participant {
            NativePipelineApplicationParticipant::Pipeline {
                definition,
                runtimes,
                ..
            } => {
                let revision = runtimes
                    .checkpoint_catalog()
                    .and_then(|catalog| catalog.root.as_ref())
                    .filter(|revision| {
                        revision.application_id > 0
                            && revision.version_id > 0
                            && revision.definition_digest == definition.definition_digest()
                    })
                    .ok_or(ApplicationExecutionError::Unavailable)?;
                (
                    revision.application_id,
                    revision.version_id,
                    revision.definition_digest,
                )
            }
            NativePipelineApplicationParticipant::ScopedAgent {
                fingerprint: Some(fingerprint),
                ..
            } => (
                fingerprint.application_id(),
                fingerprint.version_id(),
                fingerprint.definition_digest(),
            ),
            NativePipelineApplicationParticipant::ScopedAgent {
                fingerprint: None, ..
            }
            | NativePipelineApplicationParticipant::Agent(_) => {
                return Err(ApplicationExecutionError::Unavailable);
            }
        };
        let mut digest = digest::Context::new(&digest::SHA256);
        digest.update(b"elitea.graph.parallel.saved-participant.v1\0");
        digest.update(&application_id.to_be_bytes());
        digest.update(&version_id.to_be_bytes());
        digest.update(&definition_digest);
        let mut result = [0; 32];
        result.copy_from_slice(digest.finish().as_ref());
        Ok(result)
    }
}

fn rebind_participant(
    participant: &NativePipelineApplicationParticipant,
    events: &PipelineNodeEventSender,
) -> Result<NativePipelineApplicationParticipant, ApplicationExecutionError> {
    Ok(match participant {
        NativePipelineApplicationParticipant::Pipeline {
            definition,
            runtimes,
            display_name,
            ..
        } => NativePipelineApplicationParticipant::Pipeline {
            definition: definition.clone(),
            runtimes: runtimes.clone().with_parallel_events(events.clone())?,
            events: events.clone(),
            display_name: display_name.clone(),
        },
        NativePipelineApplicationParticipant::ScopedAgent {
            tool,
            scope,
            fingerprint,
        } => NativePipelineApplicationParticipant::ScopedAgent {
            tool: Arc::clone(tool),
            scope: Arc::new(scope.with_parallel_events(events.clone())),
            fingerprint: *fingerprint,
        },
        NativePipelineApplicationParticipant::Agent(_) => {
            return Err(ApplicationExecutionError::Unavailable);
        }
    })
}
