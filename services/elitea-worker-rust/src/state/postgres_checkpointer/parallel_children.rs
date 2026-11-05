#![allow(dead_code)] // Activated by the next full YAML graph compiler composition slice.

use std::sync::Arc;

use adk_rust::graph::checkpoint::Checkpointer;
use async_trait::async_trait;

use super::fanout_children::{FanoutChildKey, FanoutFamily, current_origin};
use super::{CheckpointWriterAuthority, PostgresCheckpointer};
use crate::agents::graph::fanout_budget::MAX_FANOUT_CHILDREN;
use crate::agents::graph::{
    ParallelActivation, ParallelBranchDefinition, ParallelChildCheckpoint,
    ParallelChildCheckpointerFactory, ParallelChildOrigin, ParallelChildRequest,
    PreparedChildCheckpoint,
};

#[async_trait]
impl ParallelChildCheckpointerFactory for PostgresCheckpointer {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<ParallelChildOrigin, adk_rust::graph::GraphError> {
        self.check_parallel_root(activation)?;
        current_origin(&self.scope.authority, family_of(activation))
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, adk_rust::graph::GraphError> {
        self.check_parallel_root(activation)?;
        let ordinal = branch_ordinal(ordinal)?;
        child_thread_id(self, activation, branch, ordinal, input_digest, origin)
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, adk_rust::graph::GraphError> {
        let child = self
            .activate_parallel_branch(activation, branch, ordinal, input_digest, origin)
            .await?;
        let thread_id = child.scope.authority.thread_id.clone();
        let checkpointer: Arc<dyn Checkpointer> = Arc::new(child);
        Ok(ParallelChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer,
            node_recovery: None,
        })
    }

    /// Two transactions for any child count: one batched writer activation,
    /// one `DISTINCT ON` read of every child's latest receipt.
    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, adk_rust::graph::GraphError> {
        self.check_parallel_root(activation)?;
        let mut authorities = Vec::with_capacity(children.len());
        for request in children {
            let ordinal = branch_ordinal(request.ordinal)?;
            let thread_id = child_thread_id(
                self,
                activation,
                request.branch,
                ordinal,
                request.input_digest,
                origin,
            )?;
            authorities.push(self.scope.authority.for_thread(thread_id)?);
        }
        let writers = self.activate_children(authorities).await?;
        let latest = self
            .load_children_latest(&writers.iter().collect::<Vec<_>>())
            .await?;
        Ok(writers
            .into_iter()
            .zip(latest)
            .map(|(writer, latest)| {
                let thread_id = writer.scope.authority.thread_id.clone();
                let checkpointer: Arc<dyn Checkpointer> = Arc::new(writer);
                PreparedChildCheckpoint {
                    child: ParallelChildCheckpoint {
                        admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
                        thread_id,
                        checkpointer,
                        node_recovery: None,
                    },
                    latest,
                }
            })
            .collect())
    }
}

fn branch_ordinal(ordinal: usize) -> Result<u64, adk_rust::graph::GraphError> {
    u64::try_from(ordinal).map_err(|_| {
        adk_rust::graph::GraphError::CheckpointError(
            "checkpoint.resource_exhausted: the parallel branch ordinal overflowed".to_owned(),
        )
    })
}

impl PostgresCheckpointer {
    fn check_parallel_root(
        &self,
        activation: &ParallelActivation,
    ) -> Result<(), adk_rust::graph::GraphError> {
        if activation.root_thread_id != self.scope.authority.thread_id {
            return Err(adk_rust::graph::GraphError::CheckpointError(
                "checkpoint.invalid_scope: the parallel activation is not bound to this checkpoint family"
                    .to_owned(),
            ));
        }
        Ok(())
    }

    pub(super) async fn activate_parallel_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<Self, adk_rust::graph::GraphError> {
        self.check_parallel_root(activation)?;
        let ordinal = branch_ordinal(ordinal)?;
        let thread_id = child_thread_id(self, activation, branch, ordinal, input_digest, origin)?;
        self.activate_fanout_child(thread_id).await
    }
}

fn child_thread_id(
    checkpointer: &PostgresCheckpointer,
    activation: &ParallelActivation,
    branch: &ParallelBranchDefinition,
    ordinal: u64,
    input_digest: &[u8; 32],
    origin: &ParallelChildOrigin,
) -> Result<String, adk_rust::graph::GraphError> {
    derive_child_thread_id(
        &checkpointer.scope.authority,
        activation,
        branch,
        ordinal,
        input_digest,
        origin,
    )
}

/// The family a child belongs to follows from the activation alone: only a
/// Map source sets the Map identity, and a Map child is never derived
/// without it.
const fn family_of(activation: &ParallelActivation) -> FanoutFamily {
    if activation.map.is_some() {
        FanoutFamily::Map
    } else {
        FanoutFamily::Parallel
    }
}

/// One child id per family, with each family's own fields in its own order:
/// a Map child binds main's `[node, step, config, worker, source, worker
/// node, index, item input]` (`map_children.rs`), so ids minted before the
/// Map ran on the shared adapter stay derivable.
pub(crate) fn derive_child_thread_id(
    authority: &CheckpointWriterAuthority,
    activation: &ParallelActivation,
    branch: &ParallelBranchDefinition,
    ordinal: u64,
    input_digest: &[u8; 32],
    origin: &ParallelChildOrigin,
) -> Result<String, adk_rust::graph::GraphError> {
    let step = activation.step.to_be_bytes();
    let ordinal_bytes = ordinal.to_be_bytes();
    if let Some(map) = &activation.map {
        if map.worker_digest == [0; 32]
            || map.source_digest == [0; 32]
            || activation.config_digest == [0; 32]
            || *input_digest == [0; 32]
            || usize::try_from(ordinal).map_or(true, |index| index >= MAX_FANOUT_CHILDREN)
            || branch.id() != ordinal.to_string()
            || !valid_map_worker(branch.node())
        {
            return Err(adk_rust::graph::GraphError::CheckpointError(
                "checkpoint.invalid_scope: the Map item does not belong to this original writer family"
                    .to_owned(),
            ));
        }
        return super::fanout_children::child_thread_id(
            authority,
            &FanoutChildKey {
                family: FanoutFamily::Map,
                root_thread_id: &activation.root_thread_id,
                origin,
                fields: &[
                    activation.node_id.as_bytes(),
                    &step,
                    activation.config_digest.as_slice(),
                    map.worker_digest.as_slice(),
                    map.source_digest.as_slice(),
                    branch.node().as_bytes(),
                    &ordinal_bytes,
                    input_digest,
                ],
            },
        );
    }
    super::fanout_children::child_thread_id(
        authority,
        &FanoutChildKey {
            family: FanoutFamily::Parallel,
            root_thread_id: &activation.root_thread_id,
            origin,
            fields: &[
                activation.node_id.as_bytes(),
                &step,
                activation.config_digest.as_slice(),
                branch.id().as_bytes(),
                branch.node().as_bytes(),
                &ordinal_bytes,
                input_digest,
            ],
        },
    )
}

/// The worker-name rule main's Map item check applies.
fn valid_map_worker(worker: &str) -> bool {
    !worker.is_empty()
        && worker.len() <= 128
        && worker
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':'))
}
