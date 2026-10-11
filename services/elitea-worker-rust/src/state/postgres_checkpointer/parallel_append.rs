//! Atomic expected-parent append in the existing checkpoint transaction.
//!
//! Rows are immutable per `checkpoint_id` (the exact-existing check refuses any
//! different payload under the same id), so the latest row's identity proves its
//! content. The append compares identity under the writer lock and never
//! re-reads or re-serializes the parent.

use std::collections::HashMap;

use adk_rust::graph::{Checkpoint, GraphError};
use async_trait::async_trait;
use serde_json::Value;
use sqlx::{Postgres, Row, Transaction};

use super::{
    CHECKPOINT_FAMILY, CheckpointAppendCondition, LatestIdentity, MAX_IDENTITY_BYTES,
    MAX_NODE_OR_KEY_BYTES, PostgresCheckpointError, PostgresCheckpointer, WriterLock,
    begin_transaction, bounded_identity, note_payload_bytes, note_round_trip, storage_error,
    validate_json_values,
};
use crate::agents::graph::{ParallelCheckpointAppender, ParentHead, ParentSaveProbe};

#[async_trait]
impl ParallelCheckpointAppender for PostgresCheckpointer {
    async fn append_after(
        &self,
        expected_latest: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if let Some(expected) = expected_latest {
            self.scope.require_thread(&expected.thread_id)?;
        }
        let condition =
            CheckpointAppendCondition::Latest(expected_latest.map(|expected| LatestIdentity {
                checkpoint_id: &expected.checkpoint_id,
                save_ordinal: None,
            }));
        self.persist(
            "parallel_append",
            self.save_checkpoint_inner(candidate, condition),
        )
        .await
    }

    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, GraphError> {
        self.scope.require_thread(thread_id)?;
        self.persist("parent_probe", self.probe_parent_inner(candidate_id))
            .await
    }

    async fn append_after_head(
        &self,
        expected: Option<&ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if let Some(expected) = expected {
            self.scope.require_thread(&expected.thread_id)?;
        }
        let condition =
            CheckpointAppendCondition::Latest(expected.map(|expected| LatestIdentity {
                checkpoint_id: &expected.checkpoint_id,
                save_ordinal: expected.save_ordinal,
            }));
        self.persist(
            "parallel_append",
            self.save_checkpoint_inner(candidate, condition),
        )
        .await
    }
}

impl PostgresCheckpointer {
    /// Call only after locking the current writer in the insertion transaction.
    pub(super) async fn validate_expected_latest(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        expected: Option<LatestIdentity<'_>>,
    ) -> Result<(), PostgresCheckpointError> {
        let latest = sqlx::query_as::<_, (String, i64)>(
            r"
SELECT checkpoint_id, save_ordinal
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
        note_round_trip();
        validate_expected_identity(expected, latest.as_ref())
    }

    /// One read transaction: whether `candidate_id` already exists, and the
    /// latest row's identity, frontier and metadata. Business state stays in
    /// the database.
    async fn probe_parent_inner(
        &self,
        candidate_id: &str,
    ) -> Result<ParentSaveProbe, PostgresCheckpointError> {
        if !bounded_identity(candidate_id, MAX_IDENTITY_BYTES) {
            return Err(PostgresCheckpointError::InvalidScope(
                "the requested checkpoint ID is malformed",
            ));
        }
        let mut transaction = begin_transaction(&self.pool).await?;
        self.lock_current_writer(&mut transaction, WriterLock::Shared)
            .await?;
        let row = sqlx::query(
            r"
SELECT EXISTS (
           SELECT 1
           FROM elitea_runtime.agent_graph_checkpoints
           WHERE tenant_id=$1 AND resource_project_id=$2 AND projection_project_id=$3
             AND capability_id=$4 AND checkpoint_family=$5 AND definition_digest=$6
             AND thread_id=$7 AND checkpoint_id=$8
       ) AS candidate_exists,
       latest.checkpoint_id, latest.save_ordinal, latest.step,
       latest.pending_nodes, latest.metadata
FROM (SELECT 1) AS probe
LEFT JOIN LATERAL (
    SELECT checkpoint_id, save_ordinal, step, pending_nodes, metadata
    FROM elitea_runtime.agent_graph_checkpoints
    WHERE tenant_id=$1 AND resource_project_id=$2 AND projection_project_id=$3
      AND capability_id=$4 AND checkpoint_family=$5 AND definition_digest=$6
      AND thread_id=$7
    ORDER BY save_ordinal DESC
    LIMIT 1
) AS latest ON true
",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .bind(candidate_id)
        .fetch_one(&mut *transaction)
        .await
        .map_err(storage_error)?;
        note_round_trip();
        let candidate_exists = row
            .try_get::<bool, _>("candidate_exists")
            .map_err(storage_error)?;
        let latest = match row
            .try_get::<Option<String>, _>("checkpoint_id")
            .map_err(storage_error)?
        {
            Some(checkpoint_id) => Some(self.decode_head(&row, checkpoint_id)?),
            None => None,
        };
        self.commit_current(transaction).await?;
        Ok(ParentSaveProbe {
            candidate_exists,
            latest,
        })
    }

    fn decode_head(
        &self,
        row: &sqlx::postgres::PgRow,
        checkpoint_id: String,
    ) -> Result<ParentHead, PostgresCheckpointError> {
        let text = |name: &'static str| -> Result<String, PostgresCheckpointError> {
            row.try_get::<Option<String>, _>(name)
                .map_err(storage_error)?
                .ok_or(PostgresCheckpointError::CorruptStoredState)
        };
        let number = |name: &'static str| -> Result<i64, PostgresCheckpointError> {
            row.try_get::<Option<i64>, _>(name)
                .map_err(storage_error)?
                .ok_or(PostgresCheckpointError::CorruptStoredState)
        };
        let save_ordinal = number("save_ordinal")?;
        let step = number("step")?;
        let pending_raw = text("pending_nodes")?;
        let metadata_raw = text("metadata")?;
        note_payload_bytes(pending_raw.len().saturating_add(metadata_raw.len()));
        let pending_nodes: Vec<String> = serde_json::from_str(&pending_raw)
            .map_err(|_| PostgresCheckpointError::CorruptStoredState)?;
        let metadata: HashMap<String, Value> = serde_json::from_str(&metadata_raw)
            .map_err(|_| PostgresCheckpointError::CorruptStoredState)?;
        if save_ordinal <= 0
            || !bounded_identity(&checkpoint_id, MAX_IDENTITY_BYTES)
            || pending_nodes.len() > self.limits.max_pending_nodes
            || metadata.len() > self.limits.max_map_entries
            || pending_nodes
                .iter()
                .chain(metadata.keys())
                .any(|value| !bounded_identity(value, MAX_NODE_OR_KEY_BYTES))
        {
            return Err(PostgresCheckpointError::CorruptStoredState);
        }
        validate_json_values(metadata.values(), self.limits)
            .map_err(|_| PostgresCheckpointError::CorruptStoredState)?;
        Ok(ParentHead {
            thread_id: self.scope.authority.thread_id.clone(),
            checkpoint_id,
            save_ordinal: Some(save_ordinal),
            step: usize::try_from(step).map_err(|_| PostgresCheckpointError::CorruptStoredState)?,
            pending_nodes,
            metadata,
            snapshot: None,
        })
    }
}

fn validate_expected_identity(
    expected: Option<LatestIdentity<'_>>,
    latest: Option<&(String, i64)>,
) -> Result<(), PostgresCheckpointError> {
    let equal = match (expected, latest) {
        (None, None) => true,
        (Some(expected), Some((checkpoint_id, save_ordinal))) => {
            expected.checkpoint_id == checkpoint_id
                && expected
                    .save_ordinal
                    .is_none_or(|expected| expected == *save_ordinal)
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

    fn identity(checkpoint_id: &str, save_ordinal: Option<i64>) -> LatestIdentity<'_> {
        LatestIdentity {
            checkpoint_id,
            save_ordinal,
        }
    }

    #[test]
    fn identity_comparison_refuses_another_latest_row() {
        let latest = ("parent-1".to_owned(), 7);
        validate_expected_identity(Some(identity("parent-1", None)), Some(&latest)).unwrap();
        validate_expected_identity(Some(identity("parent-1", Some(7))), Some(&latest)).unwrap();
        for expected in [
            identity("parent-0", None),
            identity("parent-0", Some(7)),
            // Same id at another ordinal: the row was deleted and re-inserted.
            identity("parent-1", Some(6)),
        ] {
            assert!(matches!(
                validate_expected_identity(Some(expected), Some(&latest)),
                Err(PostgresCheckpointError::CheckpointConflict)
            ));
        }
    }

    #[test]
    fn expected_empty_thread_refuses_a_competing_first_checkpoint() {
        let latest = ("parent-1".to_owned(), 1);
        validate_expected_identity(None, None).unwrap();
        assert!(validate_expected_identity(None, Some(&latest)).is_err());
        assert!(validate_expected_identity(Some(identity("parent-1", None)), None).is_err());
    }
}
