//! Separate a new pipeline turn from replay of its durable graph frontier.

use std::collections::HashMap;
use std::sync::Arc;

use adk_graph::checkpoint::RetentionPolicy;
use adk_graph::{Checkpoint, Checkpointer, GraphError};
use async_trait::async_trait;
use serde_json::{Value, json};

const EXECUTION_KEY: &str = "elitea.pipeline.execution.v1";

/// The execution that writes one pipeline turn's root-thread checkpoints.
///
/// Every root-thread write carries it as `elitea.pipeline.execution.v1`. A
/// fresh turn sees only its own execution's frontier; an explicit
/// continuation reads the frontier an earlier execution left.
#[derive(Clone, Debug, PartialEq)]
pub struct TurnExecution(Value);

impl TurnExecution {
    #[must_use]
    pub fn new(execution_id: &str, generation: u64) -> Self {
        Self(json!([execution_id, generation]))
    }

    /// Record this execution as the writer of `checkpoint`.
    pub fn stamp(&self, checkpoint: &mut Checkpoint) {
        checkpoint
            .metadata
            .insert(EXECUTION_KEY.to_owned(), self.0.clone());
    }

    /// Whether checkpoint metadata names this execution as its writer.
    #[must_use]
    pub fn wrote(&self, metadata: &HashMap<String, Value>) -> bool {
        metadata.get(EXECUTION_KEY) == Some(&self.0)
    }
}

/// Keep stored checkpoints intact. Fresh turns cannot load an older execution.
/// Explicit continuations still use the checkpoint validated by resume admission.
pub struct TurnCheckpointer {
    inner: Arc<dyn Checkpointer>,
    execution: TurnExecution,
    resume: bool,
}

impl TurnCheckpointer {
    pub fn new(
        inner: Arc<dyn Checkpointer>,
        execution_id: &str,
        generation: u64,
        resume: bool,
    ) -> Self {
        Self {
            inner,
            execution: TurnExecution::new(execution_id, generation),
            resume,
        }
    }
}

#[async_trait]
impl Checkpointer for TurnCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let mut checkpoint = checkpoint.clone();
        self.execution.stamp(&mut checkpoint);
        self.inner.save(&checkpoint).await
    }

    async fn load(&self, thread_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .inner
            .load(thread_id)
            .await?
            .filter(|checkpoint| self.resume || self.execution.wrote(&checkpoint.metadata)))
    }

    async fn load_by_id(&self, checkpoint_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load_by_id(checkpoint_id).await
    }

    async fn list(&self, thread_id: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread_id).await
    }

    async fn delete(&self, thread_id: &str) -> Result<(), GraphError> {
        self.inner.delete(thread_id).await
    }

    async fn prune(&self, thread_id: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread_id, policy).await
    }
}

#[cfg(test)]
mod tests {
    use adk_graph::{MemoryCheckpointer, State};

    use super::*;

    #[tokio::test]
    async fn fresh_turn_keeps_old_frontiers_but_reclaim_restores_its_own() {
        for pending in [vec![], vec!["authorize".to_owned()]] {
            let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
            let first = TurnCheckpointer::new(inner.clone(), "first", 1, false);
            let checkpoint = Checkpoint::new("thread", State::new(), 2, pending);
            let id = first.save(&checkpoint).await.expect("save first");
            let reclaimed = TurnCheckpointer::new(inner.clone(), "first", 1, false);
            assert_eq!(
                reclaimed
                    .load("thread")
                    .await
                    .expect("reclaim")
                    .expect("checkpoint")
                    .checkpoint_id,
                id
            );

            for (execution, generation) in [("second", 1), ("first", 2)] {
                let fresh = TurnCheckpointer::new(inner.clone(), execution, generation, false);
                assert!(fresh.load("thread").await.expect("fresh load").is_none());
                assert!(
                    inner
                        .load_by_id(&id)
                        .await
                        .expect("old checkpoint")
                        .is_some()
                );
                let next = Checkpoint::new("thread", State::new(), 0, vec![]);
                fresh.save(&next).await.expect("save next");
                assert_eq!(
                    fresh
                        .load("thread")
                        .await
                        .expect("reload")
                        .expect("next checkpoint")
                        .checkpoint_id,
                    next.checkpoint_id
                );
            }
        }
    }

    #[test]
    fn a_turn_execution_stamp_names_exactly_one_execution_and_generation() {
        let execution = TurnExecution::new("execution-1", 2);
        let mut checkpoint = Checkpoint::new("thread", State::new(), 0, vec![]);
        assert!(!execution.wrote(&checkpoint.metadata));
        execution.stamp(&mut checkpoint);
        assert_eq!(
            checkpoint.metadata[EXECUTION_KEY],
            json!(["execution-1", 2])
        );
        assert!(execution.wrote(&checkpoint.metadata));
        for other in [
            TurnExecution::new("execution-2", 2),
            TurnExecution::new("execution-1", 3),
        ] {
            assert!(!other.wrote(&checkpoint.metadata));
        }
    }

    #[tokio::test]
    async fn explicit_resume_reads_legacy_frontier_without_resetting_it() {
        let inner: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let legacy = Checkpoint::new("thread", State::new(), 2, vec!["authorize".to_owned()]);
        inner.save(&legacy).await.expect("legacy checkpoint");
        let fresh = TurnCheckpointer::new(inner.clone(), "new", 1, false);
        assert!(fresh.load("thread").await.expect("new turn").is_none());
        let resume = TurnCheckpointer::new(inner, "resume", 1, true);
        assert_eq!(
            resume
                .load("thread")
                .await
                .expect("resume")
                .expect("legacy")
                .checkpoint_id,
            legacy.checkpoint_id
        );
    }
}
