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

    /// The same participants, their saved pipelines journaling through one
    /// fixed Parallel branch's family (FU-248). Only participants the run
    /// lent journals to are rebound.
    pub(super) fn rebind_branch_journals(
        &self,
        journals: &Arc<dyn crate::agents::graph::node_recovery_runtime::NodeRecoveryFactory>,
    ) -> Self {
        let rebind = |participant: &NativePipelineApplicationParticipant| match participant {
            NativePipelineApplicationParticipant::Pipeline {
                definition,
                runtimes,
                events,
                display_name,
            } if runtimes.has_direct_tool_recovery() => {
                NativePipelineApplicationParticipant::Pipeline {
                    definition: definition.clone(),
                    runtimes: runtimes.clone().with_branch_journals(journals),
                    events: events.clone(),
                    display_name: display_name.clone(),
                }
            }
            other => other.clone(),
        };
        Self {
            call_owners: self.call_owners.clone(),
            nodes: self
                .nodes
                .iter()
                .map(|(node, (alias, participant))| {
                    (node.clone(), (alias.clone(), rebind(participant)))
                })
                .collect(),
            participants: self
                .participants
                .iter()
                .map(|(alias, participant)| (alias.clone(), rebind(participant)))
                .collect(),
        }
    }

    /// FU-291: the same participants, the member's own Agents running their
    /// pipeline tools on nested threads this member family admits.
    pub(super) fn rebind_tool_pipelines(
        &self,
        admission: &Arc<dyn crate::agents::graph::tool_pipeline_threads::ToolPipelineAdmission>,
    ) -> Self {
        let rebind = |participant: &NativePipelineApplicationParticipant| match participant {
            NativePipelineApplicationParticipant::ScopedAgent {
                tool,
                scope,
                fingerprint,
                member,
            } => NativePipelineApplicationParticipant::ScopedAgent {
                tool: Arc::clone(tool),
                scope: Arc::new(scope.with_tool_pipelines(Arc::clone(admission))),
                fingerprint: *fingerprint,
                member: member.clone(),
            },
            other => other.clone(),
        };
        Self {
            call_owners: self.call_owners.clone(),
            nodes: self
                .nodes
                .iter()
                .map(|(node, (alias, participant))| {
                    (node.clone(), (alias.clone(), rebind(participant)))
                })
                .collect(),
            participants: self
                .participants
                .iter()
                .map(|(alias, participant)| (alias.clone(), rebind(participant)))
                .collect(),
        }
    }

    /// W3-2b: the same participants, their root-level saved pipelines run by
    /// one root fan-out member; their Code names that member in its scope.
    /// `None` (C6) when no participant has a saved Code owner: the resolver
    /// is kept as is, nothing is rebuilt per branch.
    pub(super) fn rebind_saved_code_member(
        &self,
        member: &super::saved_code_child::SavedCodeFanoutMember,
    ) -> Option<Self> {
        let owned = |participant: &NativePipelineApplicationParticipant| {
            matches!(participant, NativePipelineApplicationParticipant::Pipeline { runtimes, .. }
                if runtimes.has_saved_code_owner())
        };
        if !self
            .nodes
            .values()
            .map(|(_, participant)| participant)
            .chain(self.participants.values())
            .any(owned)
        {
            return None;
        }
        let rebind = |participant: &NativePipelineApplicationParticipant| match participant {
            NativePipelineApplicationParticipant::Pipeline {
                definition,
                runtimes,
                events,
                display_name,
            } if runtimes.has_saved_code_owner() => {
                NativePipelineApplicationParticipant::Pipeline {
                    definition: definition.clone(),
                    runtimes: runtimes.clone().with_saved_code_member(member.clone()),
                    events: events.clone(),
                    display_name: display_name.clone(),
                }
            }
            other => other.clone(),
        };
        Some(Self {
            call_owners: self.call_owners.clone(),
            nodes: self
                .nodes
                .iter()
                .map(|(node, (alias, participant))| {
                    (node.clone(), (alias.clone(), rebind(participant)))
                })
                .collect(),
            participants: self
                .participants
                .iter()
                .map(|(alias, participant)| (alias.clone(), rebind(participant)))
                .collect(),
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
        // T-2A3: each fan-out member is its own invocation of the saved Agent,
        // with its own event channel (a Map runs one node as N concurrent
        // items, like LangGraph `Send`); the built Agent stays shared.
        NativePipelineApplicationParticipant::ScopedAgent {
            tool,
            scope,
            fingerprint,
            member,
        } => {
            let (tool, scope) = match (member, scope.applications.resume_coordinator()) {
                (Some(member), Some(resume)) => {
                    let (tool, projection, member_events) =
                        member.bind(scope.applications.presentation_catalog(), resume);
                    let scope =
                        scope.for_member(events.clone(), Arc::new(projection), member_events);
                    (tool, scope)
                }
                _ => (Arc::clone(tool), scope.with_parallel_events(events.clone())),
            };
            NativePipelineApplicationParticipant::ScopedAgent {
                tool,
                scope: Arc::new(scope),
                fingerprint: *fingerprint,
                member: member.clone(),
            }
        }
        NativePipelineApplicationParticipant::Agent(_) => {
            return Err(ApplicationExecutionError::Unavailable);
        }
    })
}
