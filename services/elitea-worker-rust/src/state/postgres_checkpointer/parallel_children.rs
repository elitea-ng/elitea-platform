#![allow(dead_code)] // Activated by the next full YAML graph compiler composition slice.

use std::sync::Arc;

use adk_rust::graph::checkpoint::Checkpointer;
use async_trait::async_trait;
use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;

use super::PostgresCheckpointer;
use crate::agents::graph::{
    ParallelActivation, ParallelBranchDefinition, ParallelChildCheckpoint,
    ParallelChildCheckpointerFactory,
};

const CHILD_THREAD_DOMAIN: &[u8] = b"elitea.graph.parallel.child-thread.v1\0";

#[async_trait]
impl ParallelChildCheckpointerFactory for PostgresCheckpointer {
    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
    ) -> Result<ParallelChildCheckpoint, adk_rust::graph::GraphError> {
        let child = self
            .activate_parallel_branch(activation, branch, ordinal, input_digest)
            .await?;
        let thread_id = child.scope.authority.thread_id.clone();
        let checkpointer: Arc<dyn Checkpointer> = Arc::new(child);
        Ok(ParallelChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer,
        })
    }
}

impl PostgresCheckpointer {
    pub(super) async fn activate_parallel_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
    ) -> Result<Self, adk_rust::graph::GraphError> {
        if activation.root_thread_id != self.scope.authority.thread_id {
            return Err(adk_rust::graph::GraphError::CheckpointError(
                "checkpoint.invalid_scope: the parallel activation is not bound to this checkpoint family"
                    .to_owned(),
            ));
        }
        let ordinal = u64::try_from(ordinal).map_err(|_| {
            adk_rust::graph::GraphError::CheckpointError(
                "checkpoint.resource_exhausted: the parallel branch ordinal overflowed".to_owned(),
            )
        })?;
        let thread_id = child_thread_id(self, activation, branch, ordinal, input_digest);
        let authority = self.scope.authority.for_thread(thread_id.clone())?;
        let child = PostgresCheckpointer::activate_under_root(
            self.pool.clone(),
            authority,
            &self.run_root_thread_id,
            self.limits,
            Arc::clone(&self.state_writer_lease),
        )
        .await?;
        Ok(child)
    }
}

fn child_thread_id(
    checkpointer: &PostgresCheckpointer,
    activation: &ParallelActivation,
    branch: &ParallelBranchDefinition,
    ordinal: u64,
    input_digest: &[u8; 32],
) -> String {
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(CHILD_THREAD_DOMAIN);
    digest_field(
        &mut context,
        checkpointer.scope.authority.tenant_id.as_bytes(),
    );
    digest_field(
        &mut context,
        &checkpointer
            .scope
            .authority
            .resource_project_id
            .to_be_bytes(),
    );
    digest_field(
        &mut context,
        &checkpointer
            .scope
            .authority
            .projection_project_id
            .to_be_bytes(),
    );
    digest_field(
        &mut context,
        checkpointer.scope.authority.capability_id.as_bytes(),
    );
    digest_field(
        &mut context,
        checkpointer.scope.authority.definition_digest.as_slice(),
    );
    digest_field(&mut context, activation.root_thread_id.as_bytes());
    digest_field(
        &mut context,
        checkpointer.scope.authority.execution_id.as_bytes(),
    );
    digest_field(
        &mut context,
        &checkpointer.scope.authority.generation.to_be_bytes(),
    );
    digest_field(&mut context, activation.node_id.as_bytes());
    digest_field(&mut context, &activation.step.to_be_bytes());
    digest_field(&mut context, activation.config_digest.as_slice());
    digest_field(&mut context, branch.id().as_bytes());
    digest_field(&mut context, branch.node().as_bytes());
    digest_field(&mut context, &ordinal.to_be_bytes());
    digest_field(&mut context, input_digest);
    let digest = context.finish();
    format!("p1:{}", URL_SAFE_NO_PAD.encode(digest.as_ref()))
}

fn digest_field(context: &mut digest::Context, value: &[u8]) {
    let length = u64::try_from(value.len()).unwrap_or(u64::MAX);
    context.update(&length.to_be_bytes());
    context.update(value);
}
