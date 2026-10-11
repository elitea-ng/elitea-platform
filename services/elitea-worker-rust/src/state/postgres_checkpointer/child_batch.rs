//! Batched child writer activation and latest-checkpoint reads.
//!
//! Fan-out preparation costs two transactions regardless of the child count:
//! one activates every admitted child writer under the run's root writer, one
//! reads every child's latest row. Each row keeps the single-thread fencing
//! predicate, and any child not returned is `WriterNotCurrent`.

use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;

use adk_rust::graph::Checkpoint;

use super::{
    CHECKPOINT_FAMILY, CheckpointWriterAuthority, PostgresCheckpointError, PostgresCheckpointer,
    begin_transaction, note_round_trip, persist_scoped, storage_error,
};

use elitea_agent_runtime::graph::fanout_budget::{
    MAX_CHILD_THREADS, MAX_FANOUT_CHILDREN, MAX_PARALLEL_BRANCHES,
};

/// Writers one activation may open: every Parallel branch with all its threads.
pub(crate) const MAX_BATCHED_WRITERS: usize = MAX_PARALLEL_BRANCHES * MAX_CHILD_THREADS;
/// Latest rows one read may return: the largest fan-out.
pub(crate) const MAX_BATCHED_READS: usize = MAX_FANOUT_CHILDREN;

impl PostgresCheckpointer {
    /// Activate every child writer in one transaction, in request order.
    /// Every authority must be this writer's claim on a distinct child thread.
    pub(super) async fn activate_children(
        &self,
        authorities: Vec<CheckpointWriterAuthority>,
    ) -> Result<Vec<Self>, PostgresCheckpointError> {
        persist_scoped("activate_children", &self.io, async {
            self.activate_children_inner(authorities).await
        })
        .await
    }

    #[allow(clippy::too_many_lines)] // Keep claim checks, root fence and the batched upsert in one transaction.
    async fn activate_children_inner(
        &self,
        authorities: Vec<CheckpointWriterAuthority>,
    ) -> Result<Vec<Self>, PostgresCheckpointError> {
        let parent = &self.scope.authority;
        let mut threads = BTreeSet::new();
        if authorities.is_empty() || authorities.len() > MAX_BATCHED_WRITERS {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the batched child activation count is outside its bound",
            ));
        }
        for authority in &authorities {
            authority.validate()?;
            if !same_claim(parent, authority)
                || authority.thread_id == self.run_root_thread_id
                || !threads.insert(authority.thread_id.clone())
            {
                return Err(PostgresCheckpointError::InvalidScope(
                    "a batched child writer is not a distinct thread of this claim",
                ));
            }
        }
        // Sorted, so concurrent activations lock writer rows in one order.
        let threads = threads.into_iter().collect::<Vec<_>>();
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresCheckpointError::WriterNotCurrent)?;
        let mut transaction = begin_transaction(&self.pool).await?;
        let root_writer = sqlx::query_scalar::<_, String>(
            r"
SELECT writer_claim_id
FROM elitea_runtime.agent_graph_checkpoint_writers
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
FOR SHARE
            ",
        )
        .bind(&parent.tenant_id)
        .bind(parent.resource_project_id)
        .bind(parent.projection_project_id)
        .bind(parent.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(parent.definition_digest.as_slice())
        .bind(&self.run_root_thread_id)
        .fetch_optional(&mut *transaction)
        .await
        .map_err(storage_error)?;
        note_round_trip();
        if root_writer.as_deref() != Some(parent.claim_id.as_str()) {
            return Err(PostgresCheckpointError::WriterNotCurrent);
        }
        let activated = sqlx::query_scalar::<_, String>(
            r"
INSERT INTO elitea_runtime.agent_graph_checkpoint_writers AS writer (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    checkpoint_family, definition_digest, thread_id, writer_claim_id,
    writer_execution_id, writer_generation, writer_claim_attempt,
    writer_lease_epoch, writer_claimed_at
)
SELECT $1, $2, $3, $4, $5, $6, child.thread_id, $8, $9, $10, $11, $12, $13
FROM unnest($7::text[]) WITH ORDINALITY AS child(thread_id, position)
ORDER BY child.position
ON CONFLICT (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    checkpoint_family, definition_digest, thread_id
)
DO UPDATE SET
    writer_claim_id = EXCLUDED.writer_claim_id,
    writer_execution_id = EXCLUDED.writer_execution_id,
    writer_generation = EXCLUDED.writer_generation,
    writer_claim_attempt = EXCLUDED.writer_claim_attempt,
    writer_lease_epoch = EXCLUDED.writer_lease_epoch,
    writer_claimed_at = EXCLUDED.writer_claimed_at,
    activated_at = clock_timestamp()
WHERE writer.writer_claim_id = EXCLUDED.writer_claim_id
   OR writer.writer_claimed_at < EXCLUDED.writer_claimed_at
RETURNING thread_id
            ",
        )
        .bind(&parent.tenant_id)
        .bind(parent.resource_project_id)
        .bind(parent.projection_project_id)
        .bind(parent.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(parent.definition_digest.as_slice())
        .bind(&threads)
        .bind(&parent.claim_id)
        .bind(&parent.execution_id)
        .bind(parent.generation)
        .bind(parent.claim_attempt)
        .bind(parent.lease_epoch)
        .bind(parent.claim_started_at)
        .fetch_all(&mut *transaction)
        .await
        .map_err(storage_error)?;
        note_round_trip();
        // A row a newer claim owns is not returned: this claim is superseded.
        if activated.into_iter().collect::<BTreeSet<_>>() != threads.iter().cloned().collect() {
            return Err(PostgresCheckpointError::WriterNotCurrent);
        }
        self.commit_current(transaction).await?;
        Ok(authorities
            .into_iter()
            .map(|authority| Self {
                pool: self.pool.clone(),
                scope: super::CheckpointScope { authority },
                limits: self.limits,
                state_writer_lease: Arc::clone(&self.state_writer_lease),
                run_root_thread_id: self.run_root_thread_id.clone(),
                io: Arc::clone(&self.io),
            })
            .collect())
    }

    /// Read every child's latest checkpoint in one transaction, in input order.
    /// All children must be writers of this claim activated by `activate_children`.
    pub(super) async fn load_children_latest(
        &self,
        children: &[&Self],
    ) -> Result<Vec<Option<Checkpoint>>, PostgresCheckpointError> {
        persist_scoped("load_children", &self.io, async {
            self.load_children_latest_inner(children).await
        })
        .await
    }

    #[allow(clippy::too_many_lines)] // Keep the batched writer fence and receipt read in one transaction.
    async fn load_children_latest_inner(
        &self,
        children: &[&Self],
    ) -> Result<Vec<Option<Checkpoint>>, PostgresCheckpointError> {
        let parent = &self.scope.authority;
        let mut threads = BTreeSet::new();
        if children.is_empty() || children.len() > MAX_BATCHED_READS {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the batched child read count is outside its bound",
            ));
        }
        for child in children {
            if !same_claim(parent, &child.scope.authority)
                || !threads.insert(child.scope.authority.thread_id.clone())
            {
                return Err(PostgresCheckpointError::InvalidScope(
                    "a batched child reader is not a distinct thread of this claim",
                ));
            }
        }
        let threads = threads.into_iter().collect::<Vec<_>>();
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresCheckpointError::WriterNotCurrent)?;
        let mut transaction = begin_transaction(&self.pool).await?;
        let current = sqlx::query_scalar::<_, String>(
            r"
SELECT writer.thread_id
FROM elitea_runtime.agent_graph_checkpoint_writers AS writer
WHERE writer.tenant_id = $1
  AND writer.resource_project_id = $2
  AND writer.projection_project_id = $3
  AND writer.capability_id = $4
  AND writer.checkpoint_family = $5
  AND writer.definition_digest = $6
  AND writer.thread_id = ANY($7::text[])
  AND writer.writer_claim_id = $8
  AND writer.writer_execution_id = $9
  AND writer.writer_generation = $10
  AND writer.writer_claim_attempt = $11
  AND writer.writer_lease_epoch = $12
FOR SHARE OF writer
            ",
        )
        .bind(&parent.tenant_id)
        .bind(parent.resource_project_id)
        .bind(parent.projection_project_id)
        .bind(parent.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(parent.definition_digest.as_slice())
        .bind(&threads)
        .bind(&parent.claim_id)
        .bind(&parent.execution_id)
        .bind(parent.generation)
        .bind(parent.claim_attempt)
        .bind(parent.lease_epoch)
        .fetch_all(&mut *transaction)
        .await
        .map_err(storage_error)?;
        note_round_trip();
        // Database collation orders differently from bytes: compare as sets.
        if current.into_iter().collect::<BTreeSet<_>>() != threads.iter().cloned().collect() {
            return Err(PostgresCheckpointError::WriterNotCurrent);
        }
        let rows = sqlx::query(
            r"
SELECT DISTINCT ON (thread_id)
       checkpoint_id, thread_id, state, step, pending_nodes, metadata,
       created_at, created_at_rfc3339, cleared_interrupt, attempts, child_ledger, payload_bytes
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = ANY($7::text[])
ORDER BY thread_id, save_ordinal DESC
            ",
        )
        .bind(&parent.tenant_id)
        .bind(parent.resource_project_id)
        .bind(parent.projection_project_id)
        .bind(parent.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(parent.definition_digest.as_slice())
        .bind(&threads)
        .fetch_all(&mut *transaction)
        .await
        .map_err(storage_error)?;
        note_round_trip();
        let by_thread = children
            .iter()
            .map(|child| (child.scope.authority.thread_id.as_str(), *child))
            .collect::<BTreeMap<_, _>>();
        let mut latest = BTreeMap::new();
        for row in &rows {
            let thread =
                sqlx::Row::try_get::<String, _>(row, "thread_id").map_err(storage_error)?;
            let child = by_thread
                .get(thread.as_str())
                .ok_or(PostgresCheckpointError::CorruptStoredState)?;
            latest.insert(thread, child.decode_row(row)?);
        }
        self.commit_current(transaction).await?;
        Ok(children
            .iter()
            .map(|child| latest.remove(&child.scope.authority.thread_id))
            .collect())
    }
}

/// Same tenant, project, family, definition and claim; only the thread differs.
fn same_claim(parent: &CheckpointWriterAuthority, child: &CheckpointWriterAuthority) -> bool {
    parent.tenant_id == child.tenant_id
        && parent.resource_project_id == child.resource_project_id
        && parent.projection_project_id == child.projection_project_id
        && parent.capability_id == child.capability_id
        && parent.definition_digest == child.definition_digest
        && parent.execution_id == child.execution_id
        && parent.generation == child.generation
        && parent.claim_id == child.claim_id
        && parent.claim_attempt == child.claim_attempt
        && parent.lease_epoch == child.lease_epoch
        && parent.claim_started_at == child.claim_started_at
}
