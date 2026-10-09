//! Node journals retain the existing exact execution and current writer fence.

use std::sync::Arc;

use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;

use super::PostgresCheckpointer;
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::{
    NodeAttemptActivation, NodeAttemptJournal, NodeRecoveryFactory, recovery_error,
};

#[async_trait]
impl NodeRecoveryFactory for PostgresCheckpointer {
    async fn open(
        &self,
        activation: &NodeAttemptActivation,
        policy: &NodeRecoveryPolicy,
    ) -> Result<NodeAttemptJournal, adk_rust::graph::GraphError> {
        if activation.root_thread_id != self.scope.authority.thread_id {
            return Err(recovery_error("pipeline.node_recovery.invalid_scope"));
        }
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| recovery_error("pipeline.node_recovery.writer_not_current"))?;
        let authority = &self.scope.authority;
        let mut hash = digest::Context::new(&digest::SHA256);
        hash.update(b"elitea.pipeline.node-attempt-journal.v1\0");
        let encoded_policy = serde_json::to_vec(policy)
            .map_err(|_| recovery_error("pipeline.node_recovery.invalid_policy"))?;
        for field in [
            authority.tenant_id.as_bytes(),
            &authority.resource_project_id.to_be_bytes(),
            &authority.projection_project_id.to_be_bytes(),
            authority.capability_id.as_bytes(),
            &authority.definition_digest,
            authority.execution_id.as_bytes(),
            &authority.generation.to_be_bytes(),
            activation.root_thread_id.as_bytes(),
            activation.node_id.as_bytes(),
            &activation.step.to_be_bytes(),
            &activation.node_digest,
            &activation.input_digest,
            &encoded_policy,
        ] {
            hash.update(
                &u64::try_from(field.len())
                    .map_err(|_| recovery_error("pipeline.node_recovery.invalid_scope"))?
                    .to_be_bytes(),
            );
            hash.update(field);
        }
        let mut activation_id = [0; 32];
        activation_id.copy_from_slice(hash.finish().as_ref());
        let thread_id = format!("n1:{}", URL_SAFE_NO_PAD.encode(activation_id));
        // A journal may start an effect, so it opens only while this claim still owns the run.
        let child = Self::activate_under_root(
            self.pool.clone(),
            authority.for_thread(thread_id.clone())?,
            &authority.thread_id,
            self.limits,
            Arc::clone(&self.state_writer_lease),
        )
        .await?;
        Ok(NodeAttemptJournal::bound(
            Arc::new(child),
            Arc::clone(&self.state_writer_lease),
            thread_id,
            activation_id,
            policy.clone(),
        ))
    }
}
