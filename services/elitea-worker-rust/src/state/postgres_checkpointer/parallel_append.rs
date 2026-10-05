//! Atomic expected-parent append in the existing checkpoint transaction.

use adk_rust::graph::{Checkpoint, GraphError};
use async_trait::async_trait;
use sqlx::{Postgres, Transaction};
use tracing::Instrument as _;

use super::{
    CHECKPOINT_FAMILY, CheckpointAppendCondition, CheckpointLimits, PostgresCheckpointError,
    PostgresCheckpointer, SerializedCheckpoint, checkpoint_operation_span,
    record_checkpoint_result, storage_error,
};
use crate::agents::graph::ParallelCheckpointAppender;

#[async_trait]
impl ParallelCheckpointAppender for PostgresCheckpointer {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let span = checkpoint_operation_span("parallel_append");
        let result = self
            .save_checkpoint_inner(
                candidate,
                CheckpointAppendCondition::Latest(expected_latest),
            )
            .instrument(span.clone())
            .await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }
}

impl PostgresCheckpointer {
    /// Call only after locking the current writer in the insertion transaction.
    pub(super) async fn validate_expected_latest(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        expected: Option<&Checkpoint>,
    ) -> Result<(), PostgresCheckpointError> {
        let row = sqlx::query(
            r"
SELECT checkpoint_id, thread_id, state, step, pending_nodes, metadata,
       created_at, created_at_rfc3339, cleared_interrupt, attempts, child_ledger
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id=$1 AND resource_project_id=$2 AND projection_project_id=$3
  AND capability_id=$4 AND checkpoint_family=$5 AND definition_digest=$6 AND thread_id=$7
ORDER BY save_ordinal DESC LIMIT 1
",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .fetch_optional(&mut **transaction)
        .await
        .map_err(storage_error)?;
        let latest = row.as_ref().map(|row| self.decode_row(row)).transpose()?;
        validate_expected_checkpoint(expected, latest.as_ref(), self.limits)
    }
}

fn validate_expected_checkpoint(
    expected: Option<&Checkpoint>,
    latest: Option<&Checkpoint>,
    limits: CheckpointLimits,
) -> Result<(), PostgresCheckpointError> {
    let equal = match (expected, latest) {
        (None, None) => true,
        (Some(expected), Some(latest)) => {
            expected.thread_id == latest.thread_id
                && expected.checkpoint_id == latest.checkpoint_id
                && expected.created_at == latest.created_at
                && expected.cleared_interrupt == latest.cleared_interrupt
                && SerializedCheckpoint::new(expected, limits)?
                    == SerializedCheckpoint::new(latest, limits)?
        }
        _ => false,
    };
    if !equal {
        return Err(PostgresCheckpointError::CheckpointConflict);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;

    use serde_json::json;

    #[test]
    fn complete_parent_comparison_rejects_every_changed_checkpoint_field() {
        let mut original = Checkpoint::new(
            "parent",
            HashMap::from([("business".to_owned(), json!({"value": 1}))]),
            4,
            vec!["parallel".to_owned()],
        );
        original
            .metadata
            .insert("receipt".to_owned(), json!("original"));
        original.attempts.insert("parallel".to_owned(), 1);
        original
            .child_ledger
            .insert("owned".to_owned(), json!("original"));
        original.cleared_interrupt = Some("parallel".to_owned());
        let limits = CheckpointLimits::default();
        validate_expected_checkpoint(Some(&original), Some(&original), limits).unwrap();
        for field in 0..10 {
            let mut newer = original.clone();
            match field {
                0 => newer.thread_id.push_str("-other"),
                1 => newer.checkpoint_id.push_str("-other"),
                2 => newer.created_at += chrono::Duration::nanoseconds(1),
                3 => newer.cleared_interrupt = None,
                4 => newer.step += 1,
                5 => newer.pending_nodes = vec!["after".to_owned()],
                6 => {
                    newer.state.insert("business".to_owned(), json!("newer"));
                }
                7 => {
                    newer.metadata.insert("receipt".to_owned(), json!("newer"));
                }
                8 => {
                    newer.attempts.insert("parallel".to_owned(), 2);
                }
                9 => {
                    newer
                        .child_ledger
                        .insert("owned".to_owned(), json!("newer"));
                }
                _ => unreachable!(),
            }
            assert!(matches!(
                validate_expected_checkpoint(Some(&original), Some(&newer), limits),
                Err(PostgresCheckpointError::CheckpointConflict)
            ));
        }
    }

    #[test]
    fn expected_empty_thread_refuses_a_competing_first_checkpoint() {
        let checkpoint = Checkpoint::new("parent", HashMap::new(), 0, vec!["parallel".to_owned()]);
        let limits = CheckpointLimits::default();
        validate_expected_checkpoint(None, None, limits).unwrap();
        assert!(validate_expected_checkpoint(None, Some(&checkpoint), limits).is_err());
        assert!(validate_expected_checkpoint(Some(&checkpoint), None, limits).is_err());
    }
}
