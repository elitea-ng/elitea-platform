//! Route only admitted application-node threads to independently fenced adapters.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;

use super::{PostgresCheckpointError, PostgresCheckpointer};

pub(crate) struct ApplicationCheckpointers {
    threads: BTreeMap<String, PostgresCheckpointer>,
}

impl PostgresCheckpointer {
    pub(crate) async fn with_application_children(
        self,
        node_ids: impl Iterator<Item = &str>,
    ) -> Result<ApplicationCheckpointers, PostgresCheckpointError> {
        let mut threads = BTreeMap::new();
        for node_id in node_ids {
            // Match ADK SubgraphNode::child_thread. IDs come from admitted YAML.
            if threads.len() >= 128 || node_id.is_empty() || node_id.contains('/') {
                return Err(PostgresCheckpointError::InvalidScope(
                    "the application checkpoint node is invalid",
                ));
            }
            let thread_id = format!("{}/{node_id}", self.scope.authority.thread_id);
            let authority = self.scope.authority.for_thread(thread_id.clone())?;
            let child = Self::activate(
                self.pool.clone(),
                authority,
                self.limits,
                Arc::clone(&self.state_writer_lease),
            )
            .await?;
            if threads.insert(thread_id, child).is_some() {
                return Err(PostgresCheckpointError::InvalidScope(
                    "the application checkpoint node is duplicated",
                ));
            }
        }
        threads.insert(self.scope.authority.thread_id.clone(), self);
        Ok(ApplicationCheckpointers { threads })
    }
}

impl ApplicationCheckpointers {
    fn for_thread(&self, thread_id: &str) -> Result<&PostgresCheckpointer, GraphError> {
        self.threads.get(thread_id).ok_or_else(|| {
            PostgresCheckpointError::InvalidScope(
                "the requested thread is not an admitted application checkpoint",
            )
            .into()
        })
    }
}

#[async_trait]
impl Checkpointer for ApplicationCheckpointers {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.for_thread(&checkpoint.thread_id)?
            .save(checkpoint)
            .await
    }

    async fn load(&self, thread_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.for_thread(thread_id)?.load(thread_id).await
    }

    async fn load_by_id(&self, checkpoint_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        // Every lookup remains restricted to one admitted, fenced lineage.
        for checkpointer in self.threads.values() {
            if let Some(checkpoint) = checkpointer.load_by_id(checkpoint_id).await? {
                return Ok(Some(checkpoint));
            }
        }
        Ok(None)
    }

    async fn list(&self, thread_id: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.for_thread(thread_id)?.list(thread_id).await
    }

    async fn delete(&self, thread_id: &str) -> Result<(), GraphError> {
        self.for_thread(thread_id)?.delete(thread_id).await
    }

    async fn prune(&self, thread_id: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.for_thread(thread_id)?.prune(thread_id, policy).await
    }
}
