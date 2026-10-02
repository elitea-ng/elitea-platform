use std::collections::{BTreeMap, HashMap};
use std::fmt;
use std::io::{self, Write};
use std::sync::Arc;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, GraphError, State};
use async_trait::async_trait;
use chrono::{DateTime, SecondsFormat, TimeZone as _, Utc};
use serde_json::Value;
use sqlx::{PgPool, Postgres, Row, Transaction};
use thiserror::Error;
use tracing::Instrument as _;
use zeroize::Zeroizing;

use super::StateWriterLease;

mod application_children;
mod parallel_children;

const CHECKPOINT_FAMILY: &str = "adk-graph.2.0.0.v1";
const MAX_DATABASE_PAYLOAD_BYTES: usize = 8 * 1024 * 1024;
const MAX_DATABASE_PENDING_NODES: usize = 4096;
const MAX_DATABASE_MAP_ENTRIES: usize = 4096;
const MAX_DATABASE_CHECKPOINTS_PER_THREAD: usize = 4096;
const MAX_DATABASE_RETAINED_BYTES: usize = 64 * 1024 * 1024;
const MAX_DATABASE_JSON_DEPTH: usize = 64;
const MAX_DATABASE_JSON_NODES: usize = 65_536;
#[allow(dead_code)] // Used once the sealed invocation coordinator constructs writer authority.
const MAX_TENANT_BYTES: usize = 256;
const MAX_THREAD_BYTES: usize = 512;
const MAX_IDENTITY_BYTES: usize = 256;
const MAX_NODE_OR_KEY_BYTES: usize = 512;
#[allow(dead_code)] // Used once the sealed invocation coordinator constructs writer authority.
pub(super) const APPLICATION_CAPABILITY_ID: &str = "agent.execute.application.v1";
#[allow(dead_code)] // Used once the sealed invocation coordinator constructs writer authority.
const ADHOC_CAPABILITY_ID: &str = "agent.execute.adhoc.v1";

/// Resource limits enforced before serialization reaches `PostgreSQL`.
///
/// The database migration carries the same hard ceilings. Runtime limits may
/// be lower for a deployment, but never higher, so a worker cannot admit data
/// that another worker would be unable to restore safely.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CheckpointLimits {
    pub max_serialized_bytes: usize,
    pub max_pending_nodes: usize,
    pub max_map_entries: usize,
    pub max_checkpoints_per_thread: usize,
    pub max_retained_bytes: usize,
    pub max_json_depth: usize,
    pub max_json_nodes: usize,
}

impl Default for CheckpointLimits {
    fn default() -> Self {
        Self {
            max_serialized_bytes: MAX_DATABASE_PAYLOAD_BYTES,
            max_pending_nodes: MAX_DATABASE_PENDING_NODES,
            max_map_entries: MAX_DATABASE_MAP_ENTRIES,
            max_checkpoints_per_thread: MAX_DATABASE_CHECKPOINTS_PER_THREAD,
            max_retained_bytes: MAX_DATABASE_RETAINED_BYTES,
            max_json_depth: MAX_DATABASE_JSON_DEPTH,
            max_json_nodes: MAX_DATABASE_JSON_NODES,
        }
    }
}

impl CheckpointLimits {
    fn validate(self) -> Result<Self, PostgresCheckpointError> {
        if self.max_serialized_bytes == 0
            || self.max_serialized_bytes > MAX_DATABASE_PAYLOAD_BYTES
            || self.max_pending_nodes == 0
            || self.max_pending_nodes > MAX_DATABASE_PENDING_NODES
            || self.max_map_entries == 0
            || self.max_map_entries > MAX_DATABASE_MAP_ENTRIES
            || self.max_checkpoints_per_thread == 0
            || self.max_checkpoints_per_thread > MAX_DATABASE_CHECKPOINTS_PER_THREAD
            || self.max_retained_bytes == 0
            || self.max_retained_bytes > MAX_DATABASE_RETAINED_BYTES
            || self.max_json_depth == 0
            || self.max_json_depth > MAX_DATABASE_JSON_DEPTH
            || self.max_json_nodes == 0
            || self.max_json_nodes > MAX_DATABASE_JSON_NODES
        {
            return Err(PostgresCheckpointError::InvalidConfiguration(
                "the PostgreSQL checkpoint limits are outside the supported profile",
            ));
        }
        Ok(self)
    }
}

/// Typed, data-free failure from the `PostgreSQL` checkpoint boundary.
///
/// `Display` is safe for operator logs and ADK error propagation. The database
/// source remains available through [`std::error::Error::source`] for a trusted
/// diagnostics sink, but SQL text and server details are never put in the
/// public message.
#[derive(Debug, Error)]
pub enum PostgresCheckpointError {
    #[error("the PostgreSQL checkpoint configuration is invalid: {0}")]
    InvalidConfiguration(&'static str),
    #[error("the PostgreSQL checkpoint identity or thread scope is invalid: {0}")]
    InvalidScope(&'static str),
    #[error("the PostgreSQL checkpoint exceeds an approved resource limit: {0}")]
    ResourceExhausted(&'static str),
    #[error("the PostgreSQL checkpoint writer is no longer current")]
    WriterNotCurrent,
    #[error("the PostgreSQL checkpoint ID conflicts with different durable state")]
    CheckpointConflict,
    #[error("the PostgreSQL checkpoint row is malformed or incompatible")]
    CorruptStoredState,
    #[error("PostgreSQL checkpoint storage is temporarily unavailable")]
    StorageUnavailable {
        #[source]
        source: sqlx::Error,
    },
    #[error("the PostgreSQL checkpoint operation failed")]
    StorageFailure {
        #[source]
        source: sqlx::Error,
    },
}

impl PostgresCheckpointError {
    /// Stable, low-cardinality code for tracing and terminal classification.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidConfiguration(_) => "checkpoint.invalid_configuration",
            Self::InvalidScope(_) => "checkpoint.invalid_scope",
            Self::ResourceExhausted(_) => "checkpoint.resource_exhausted",
            Self::WriterNotCurrent => "checkpoint.writer_not_current",
            Self::CheckpointConflict => "checkpoint.conflict",
            Self::CorruptStoredState => "checkpoint.corrupt_state",
            Self::StorageUnavailable { .. } => "checkpoint.storage_unavailable",
            Self::StorageFailure { .. } => "checkpoint.storage_failure",
        }
    }

    /// Whether replay under the same immutable command can reasonably succeed.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(self, Self::StorageUnavailable { .. })
    }
}

impl From<PostgresCheckpointError> for GraphError {
    fn from(error: PostgresCheckpointError) -> Self {
        Self::CheckpointError(format!("{}: {error}", error.code()))
    }
}

/// Claim-bound identity used to activate one thread writer.
///
/// This type is crate-private, non-cloneable and intentionally has no `Debug`.
/// Main's authenticated claim receipt supplies the database-authored creation
/// time used to order writer takeover. The full bearer fence is kept in a
/// zeroizing, non-debug field and never written to either checkpoint table.
pub(crate) struct CheckpointWriterAuthority {
    tenant_id: String,
    resource_project_id: i32,
    projection_project_id: i32,
    capability_id: &'static str,
    definition_digest: [u8; 32],
    thread_id: String,
    execution_id: String,
    generation: i64,
    claim_id: String,
    claim_attempt: i64,
    lease_epoch: i64,
    claim_started_at: DateTime<Utc>,
    workload_session_id: String,
    producer_id: String,
    fence_token: Zeroizing<[u8; 32]>,
}

impl CheckpointWriterAuthority {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn new(
        tenant_id: String,
        resource_project_id: i32,
        projection_project_id: i32,
        capability_id: &'static str,
        definition_digest: [u8; 32],
        thread_id: String,
        execution_id: String,
        generation: u64,
        claim_id: String,
        claim_attempt: u64,
        lease_epoch: u64,
        claim_started_at_unix_micros: i64,
        workload_session_id: String,
        producer_id: String,
        fence_token: [u8; 32],
    ) -> Result<Self, PostgresCheckpointError> {
        let generation = i64::try_from(generation).map_err(|_| {
            PostgresCheckpointError::InvalidScope(
                "the checkpoint generation is outside PostgreSQL BIGINT",
            )
        })?;
        let claim_attempt = i64::try_from(claim_attempt).map_err(|_| {
            PostgresCheckpointError::InvalidScope(
                "the checkpoint claim attempt is outside PostgreSQL BIGINT",
            )
        })?;
        let lease_epoch = i64::try_from(lease_epoch).map_err(|_| {
            PostgresCheckpointError::InvalidScope(
                "the checkpoint lease epoch is outside PostgreSQL BIGINT",
            )
        })?;
        let claim_started_at = Utc
            .timestamp_micros(claim_started_at_unix_micros)
            .single()
            .ok_or(PostgresCheckpointError::InvalidScope(
                "the checkpoint claim creation time is malformed",
            ))?;
        let authority = Self {
            tenant_id,
            resource_project_id,
            projection_project_id,
            capability_id,
            definition_digest,
            thread_id,
            execution_id,
            generation,
            claim_id,
            claim_attempt,
            lease_epoch,
            claim_started_at,
            workload_session_id,
            producer_id,
            fence_token: Zeroizing::new(fence_token),
        };
        authority.validate()?;
        Ok(authority)
    }

    fn validate(&self) -> Result<(), PostgresCheckpointError> {
        if !bounded_identity(&self.tenant_id, MAX_TENANT_BYTES)
            || !bounded_identity(&self.thread_id, MAX_THREAD_BYTES)
            || !bounded_identity(&self.execution_id, MAX_IDENTITY_BYTES)
            || !bounded_identity(&self.claim_id, MAX_IDENTITY_BYTES)
            || !bounded_identity(&self.workload_session_id, MAX_IDENTITY_BYTES)
            || !bounded_identity(&self.producer_id, MAX_IDENTITY_BYTES)
            || self.resource_project_id <= 0
            || self.projection_project_id <= 0
            || !matches!(
                self.capability_id,
                APPLICATION_CAPABILITY_ID | ADHOC_CAPABILITY_ID
            )
            || self.definition_digest.iter().all(|byte| *byte == 0)
            || self.generation <= 0
            || self.claim_attempt <= 0
            || self.lease_epoch <= 0
            || self.claim_started_at.timestamp_micros() <= 0
            || self.fence_token.iter().all(|byte| *byte == 0)
        {
            return Err(PostgresCheckpointError::InvalidScope(
                "the checkpoint writer authority is malformed",
            ));
        }
        Ok(())
    }

    fn for_thread(&self, thread_id: String) -> Result<Self, PostgresCheckpointError> {
        let mut fence_token = [0_u8; 32];
        fence_token.copy_from_slice(self.fence_token.as_slice());
        let authority = Self {
            tenant_id: self.tenant_id.clone(),
            resource_project_id: self.resource_project_id,
            projection_project_id: self.projection_project_id,
            capability_id: self.capability_id,
            definition_digest: self.definition_digest,
            thread_id,
            execution_id: self.execution_id.clone(),
            generation: self.generation,
            claim_id: self.claim_id.clone(),
            claim_attempt: self.claim_attempt,
            lease_epoch: self.lease_epoch,
            claim_started_at: self.claim_started_at,
            workload_session_id: self.workload_session_id.clone(),
            producer_id: self.producer_id.clone(),
            fence_token: Zeroizing::new(fence_token),
        };
        authority.validate()?;
        Ok(authority)
    }
}

struct CheckpointScope {
    authority: CheckpointWriterAuthority,
}

impl CheckpointScope {
    fn require_thread(&self, thread_id: &str) -> Result<(), PostgresCheckpointError> {
        if thread_id != self.authority.thread_id {
            return Err(PostgresCheckpointError::InvalidScope(
                "the requested thread is not bound to this checkpoint adapter",
            ));
        }
        Ok(())
    }
}

/// ADK-Rust 2.0.0 [`Checkpointer`] backed by Elitea's `PostgreSQL` runtime schema.
///
/// One instance is activated for exactly one tenant/project/thread and one
/// supervised Main claim. Every operation rechecks the live lease guard and
/// writer row. A newer claim activation therefore fences an older worker even
/// when that worker still holds this Rust value.
///
/// The constructor is crate-private because raw strings are not authority. The
/// invocation coordinator will construct the writer binding only from its
/// opaque, lease-monitored claim state.
pub struct PostgresCheckpointer {
    pool: PgPool,
    scope: CheckpointScope,
    limits: CheckpointLimits,
    state_writer_lease: Arc<dyn StateWriterLease>,
}

impl PostgresCheckpointer {
    /// Activate the claim as current writer for its exact checkpoint lineage.
    ///
    /// Main's `PostgreSQL` claim creation time is carried by the authenticated
    /// receipt. An older claim cannot replace a writer whose claim was created
    /// later; equal timestamps from different claims fail closed instead of
    /// inventing an ordering.
    pub(crate) async fn activate(
        pool: PgPool,
        authority: CheckpointWriterAuthority,
        limits: CheckpointLimits,
        state_writer_lease: Arc<dyn StateWriterLease>,
    ) -> Result<Self, PostgresCheckpointError> {
        authority.validate()?;
        let limits = limits.validate()?;
        state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresCheckpointError::WriterNotCurrent)?;
        let mut transaction = pool.begin().await.map_err(storage_error)?;
        let activated = sqlx::query_scalar::<_, String>(
            r"
INSERT INTO elitea_runtime.agent_graph_checkpoint_writers AS writer (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    checkpoint_family, definition_digest, thread_id, writer_claim_id,
    writer_execution_id, writer_generation, writer_claim_attempt,
    writer_lease_epoch, writer_claimed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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
RETURNING writer_claim_id
            ",
        )
        .bind(&authority.tenant_id)
        .bind(authority.resource_project_id)
        .bind(authority.projection_project_id)
        .bind(authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(authority.definition_digest.as_slice())
        .bind(&authority.thread_id)
        .bind(&authority.claim_id)
        .bind(&authority.execution_id)
        .bind(authority.generation)
        .bind(authority.claim_attempt)
        .bind(authority.lease_epoch)
        .bind(authority.claim_started_at)
        .fetch_optional(&mut *transaction)
        .await
        .map_err(storage_error)?;
        if activated.as_deref() != Some(authority.claim_id.as_str()) {
            return Err(PostgresCheckpointError::WriterNotCurrent);
        }
        state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresCheckpointError::WriterNotCurrent)?;
        transaction.commit().await.map_err(storage_error)?;
        Ok(Self {
            pool,
            scope: CheckpointScope { authority },
            limits,
            state_writer_lease,
        })
    }

    async fn save_checkpoint(
        &self,
        checkpoint: &Checkpoint,
    ) -> Result<String, PostgresCheckpointError> {
        self.scope.require_thread(&checkpoint.thread_id)?;
        let serialized = SerializedCheckpoint::new(checkpoint, self.limits)?;
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, WriterLock::Exclusive)
            .await?;

        let exact_existing = sqlx::query_scalar::<_, bool>(
            r"
SELECT state = $9
   AND step = $10
   AND pending_nodes = $11
   AND metadata = $12
   AND created_at = $13
   AND created_at_rfc3339 = $14
   AND cleared_interrupt IS NOT DISTINCT FROM $15
   AND attempts = $16
   AND child_ledger = $17
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND checkpoint_id = $8
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .bind(&checkpoint.checkpoint_id)
        .bind(&serialized.state)
        .bind(serialized.step)
        .bind(&serialized.pending_nodes)
        .bind(&serialized.metadata)
        .bind(checkpoint.created_at)
        .bind(&serialized.created_at_rfc3339)
        .bind(checkpoint.cleared_interrupt.as_deref())
        .bind(&serialized.attempts)
        .bind(&serialized.child_ledger)
        .fetch_optional(&mut *transaction)
        .await
        .map_err(storage_error)?;
        match exact_existing {
            Some(true) => {
                self.commit_current(transaction).await?;
                return Ok(checkpoint.checkpoint_id.clone());
            }
            Some(false) => return Err(PostgresCheckpointError::CheckpointConflict),
            None => {}
        }

        self.ensure_save_capacity(&mut transaction, serialized.total_bytes)
            .await?;
        let save_ordinal = self.allocate_save_ordinal(&mut transaction).await?;
        self.insert_checkpoint(&mut transaction, checkpoint, &serialized, save_ordinal)
            .await?;
        self.commit_current(transaction).await?;
        Ok(checkpoint.checkpoint_id.clone())
    }

    async fn ensure_save_capacity(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        candidate_bytes: usize,
    ) -> Result<(), PostgresCheckpointError> {
        let (count, retained_bytes) = sqlx::query_as::<_, (i64, i64)>(
            r"
SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .fetch_one(&mut **transaction)
        .await
        .map_err(storage_error)?;
        if usize::try_from(count)
            .ok()
            .is_none_or(|count| count >= self.limits.max_checkpoints_per_thread)
        {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the thread checkpoint count reached its configured limit",
            ));
        }
        let retained_bytes = usize::try_from(retained_bytes)
            .ok()
            .and_then(|retained| retained.checked_add(candidate_bytes))
            .ok_or(PostgresCheckpointError::ResourceExhausted(
                "the retained checkpoint size overflowed",
            ))?;
        if retained_bytes > self.limits.max_retained_bytes {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the retained thread checkpoint bytes reached their configured limit",
            ));
        }
        Ok(())
    }

    async fn insert_checkpoint(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        checkpoint: &Checkpoint,
        serialized: &SerializedCheckpoint,
        save_ordinal: i64,
    ) -> Result<(), PostgresCheckpointError> {
        sqlx::query(
            r"
INSERT INTO elitea_runtime.agent_graph_checkpoints (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    checkpoint_family, definition_digest, thread_id, checkpoint_id, state,
    step, pending_nodes, metadata, created_at, created_at_rfc3339,
    cleared_interrupt, attempts, child_ledger, save_ordinal, writer_claim_id,
    writer_execution_id, writer_generation, writer_claim_attempt, writer_lease_epoch
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
    $12, $13, $14, $15, $16, $17, $18, $19, $20,
    $21, $22, $23
)
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .bind(&checkpoint.checkpoint_id)
        .bind(&serialized.state)
        .bind(serialized.step)
        .bind(&serialized.pending_nodes)
        .bind(&serialized.metadata)
        .bind(checkpoint.created_at)
        .bind(&serialized.created_at_rfc3339)
        .bind(checkpoint.cleared_interrupt.as_deref())
        .bind(&serialized.attempts)
        .bind(&serialized.child_ledger)
        .bind(save_ordinal)
        .bind(&self.scope.authority.claim_id)
        .bind(&self.scope.authority.execution_id)
        .bind(self.scope.authority.generation)
        .bind(self.scope.authority.claim_attempt)
        .bind(self.scope.authority.lease_epoch)
        .execute(&mut **transaction)
        .await
        .map_err(storage_error)?;
        Ok(())
    }

    async fn allocate_save_ordinal(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
    ) -> Result<i64, PostgresCheckpointError> {
        sqlx::query_scalar::<_, i64>(
            r"
UPDATE elitea_runtime.agent_graph_checkpoint_writers
SET next_save_ordinal = next_save_ordinal + 1
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND next_save_ordinal < 9223372036854775807
RETURNING next_save_ordinal - 1
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
        .map_err(storage_error)?
        .ok_or(PostgresCheckpointError::ResourceExhausted(
            "the checkpoint save ordinal reached PostgreSQL BIGINT",
        ))
    }

    async fn load_checkpoint(
        &self,
        checkpoint_id: Option<&str>,
    ) -> Result<Option<Checkpoint>, PostgresCheckpointError> {
        if checkpoint_id.is_some_and(|value| !bounded_identity(value, MAX_IDENTITY_BYTES)) {
            return Err(PostgresCheckpointError::InvalidScope(
                "the requested checkpoint ID is malformed",
            ));
        }
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, WriterLock::Shared)
            .await?;
        let row = if let Some(checkpoint_id) = checkpoint_id {
            sqlx::query(
                r"
SELECT checkpoint_id, thread_id, state, step, pending_nodes, metadata,
       created_at, created_at_rfc3339, cleared_interrupt, attempts, child_ledger
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND checkpoint_id = $8
                ",
            )
            .bind(&self.scope.authority.tenant_id)
            .bind(self.scope.authority.resource_project_id)
            .bind(self.scope.authority.projection_project_id)
            .bind(self.scope.authority.capability_id)
            .bind(CHECKPOINT_FAMILY)
            .bind(self.scope.authority.definition_digest.as_slice())
            .bind(&self.scope.authority.thread_id)
            .bind(checkpoint_id)
            .fetch_optional(&mut *transaction)
            .await
            .map_err(storage_error)?
        } else {
            sqlx::query(
                r"
SELECT checkpoint_id, thread_id, state, step, pending_nodes, metadata,
       created_at, created_at_rfc3339, cleared_interrupt, attempts, child_ledger
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
ORDER BY save_ordinal DESC
LIMIT 1
                ",
            )
            .bind(&self.scope.authority.tenant_id)
            .bind(self.scope.authority.resource_project_id)
            .bind(self.scope.authority.projection_project_id)
            .bind(self.scope.authority.capability_id)
            .bind(CHECKPOINT_FAMILY)
            .bind(self.scope.authority.definition_digest.as_slice())
            .bind(&self.scope.authority.thread_id)
            .fetch_optional(&mut *transaction)
            .await
            .map_err(storage_error)?
        };
        let checkpoint = row.map(|row| self.decode_row(&row)).transpose()?;
        self.commit_current(transaction).await?;
        Ok(checkpoint)
    }

    async fn list_checkpoints(&self) -> Result<Vec<Checkpoint>, PostgresCheckpointError> {
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, WriterLock::Shared)
            .await?;
        let (stored_count, stored_bytes) = sqlx::query_as::<_, (i64, i64)>(
            r"
SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .fetch_one(&mut *transaction)
        .await
        .map_err(storage_error)?;
        if usize::try_from(stored_count)
            .ok()
            .is_none_or(|count| count > self.limits.max_checkpoints_per_thread)
            || usize::try_from(stored_bytes)
                .ok()
                .is_none_or(|bytes| bytes > self.limits.max_retained_bytes)
        {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the stored thread checkpoint set exceeds its configured limit",
            ));
        }
        let limit = i64::try_from(self.limits.max_checkpoints_per_thread).map_err(|_| {
            PostgresCheckpointError::InvalidConfiguration(
                "the thread checkpoint limit is outside PostgreSQL BIGINT",
            )
        })? + 1;
        let rows = sqlx::query(
            r"
SELECT checkpoint_id, thread_id, state, step, pending_nodes, metadata,
       created_at, created_at_rfc3339, cleared_interrupt, attempts, child_ledger
FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
ORDER BY save_ordinal ASC
LIMIT $8
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .bind(limit)
        .fetch_all(&mut *transaction)
        .await
        .map_err(storage_error)?;
        if rows.len() > self.limits.max_checkpoints_per_thread {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the stored thread checkpoint count exceeds its configured limit",
            ));
        }
        let checkpoints = rows
            .iter()
            .map(|row| self.decode_row(row))
            .collect::<Result<Vec<_>, _>>()?;
        self.commit_current(transaction).await?;
        Ok(checkpoints)
    }

    async fn delete_checkpoints(&self) -> Result<(), PostgresCheckpointError> {
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, WriterLock::Exclusive)
            .await?;
        sqlx::query(
            r"
DELETE FROM elitea_runtime.agent_graph_checkpoints
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND checkpoint_family = $5
  AND definition_digest = $6
  AND thread_id = $7
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .execute(&mut *transaction)
        .await
        .map_err(storage_error)?;
        self.commit_current(transaction).await?;
        Ok(())
    }

    async fn prune_checkpoints(
        &self,
        policy: &RetentionPolicy,
    ) -> Result<usize, PostgresCheckpointError> {
        if policy.is_unlimited() {
            return Ok(0);
        }
        let max_per_thread = policy
            .max_per_thread
            .map(|count| i64::try_from(count.max(1)))
            .transpose()
            .map_err(|_| {
                PostgresCheckpointError::InvalidConfiguration(
                    "the retention count is outside PostgreSQL BIGINT",
                )
            })?;
        let max_age_micros = policy.max_age.map(duration_micros_ceil).transpose()?;
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, WriterLock::Exclusive)
            .await?;
        let result = sqlx::query(
            r"
WITH ordered AS (
    SELECT checkpoint_id,
           row_number() OVER (
               ORDER BY save_ordinal DESC
           ) AS newest_ordinal
    FROM elitea_runtime.agent_graph_checkpoints
    WHERE tenant_id = $1
      AND resource_project_id = $2
      AND projection_project_id = $3
      AND capability_id = $4
      AND checkpoint_family = $5
      AND definition_digest = $6
      AND thread_id = $7
), expired AS (
    SELECT checkpoint_id
    FROM ordered
    JOIN elitea_runtime.agent_graph_checkpoints AS checkpoint
      USING (checkpoint_id)
    WHERE checkpoint.tenant_id = $1
      AND checkpoint.resource_project_id = $2
      AND checkpoint.projection_project_id = $3
      AND checkpoint.capability_id = $4
      AND checkpoint.checkpoint_family = $5
      AND checkpoint.definition_digest = $6
      AND checkpoint.thread_id = $7
      AND ordered.newest_ordinal > 1
      AND (
          ($8::bigint IS NOT NULL AND ordered.newest_ordinal > $8)
          OR (
              $9::bigint IS NOT NULL
              AND checkpoint.created_at
                  < clock_timestamp() - ($9 * interval '1 microsecond')
          )
      )
)
DELETE FROM elitea_runtime.agent_graph_checkpoints AS checkpoint
USING expired
WHERE checkpoint.tenant_id = $1
  AND checkpoint.resource_project_id = $2
  AND checkpoint.projection_project_id = $3
  AND checkpoint.capability_id = $4
  AND checkpoint.checkpoint_family = $5
  AND checkpoint.definition_digest = $6
  AND checkpoint.thread_id = $7
  AND checkpoint.checkpoint_id = expired.checkpoint_id
            ",
        )
        .bind(&self.scope.authority.tenant_id)
        .bind(self.scope.authority.resource_project_id)
        .bind(self.scope.authority.projection_project_id)
        .bind(self.scope.authority.capability_id)
        .bind(CHECKPOINT_FAMILY)
        .bind(self.scope.authority.definition_digest.as_slice())
        .bind(&self.scope.authority.thread_id)
        .bind(max_per_thread)
        .bind(max_age_micros)
        .execute(&mut *transaction)
        .await
        .map_err(storage_error)?;
        self.commit_current(transaction).await?;
        usize::try_from(result.rows_affected()).map_err(|_| {
            PostgresCheckpointError::ResourceExhausted(
                "the pruned checkpoint count exceeds the platform integer range",
            )
        })
    }

    async fn lock_current_writer(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        lock: WriterLock,
    ) -> Result<(), PostgresCheckpointError> {
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresCheckpointError::WriterNotCurrent)?;
        let writer_query = match lock {
            WriterLock::Shared => CURRENT_WRITER_SHARED_SQL,
            WriterLock::Exclusive => CURRENT_WRITER_EXCLUSIVE_SQL,
        };
        let current_writer = sqlx::query_scalar::<_, i32>(writer_query)
            .bind(&self.scope.authority.tenant_id)
            .bind(self.scope.authority.resource_project_id)
            .bind(self.scope.authority.projection_project_id)
            .bind(self.scope.authority.capability_id)
            .bind(CHECKPOINT_FAMILY)
            .bind(self.scope.authority.definition_digest.as_slice())
            .bind(&self.scope.authority.thread_id)
            .bind(&self.scope.authority.claim_id)
            .bind(&self.scope.authority.execution_id)
            .bind(self.scope.authority.generation)
            .bind(self.scope.authority.claim_attempt)
            .bind(self.scope.authority.lease_epoch)
            .fetch_optional(&mut **transaction)
            .await
            .map_err(storage_error)?;
        if current_writer != Some(1) {
            return Err(PostgresCheckpointError::WriterNotCurrent);
        }
        Ok(())
    }

    async fn commit_current(
        &self,
        transaction: Transaction<'_, Postgres>,
    ) -> Result<(), PostgresCheckpointError> {
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresCheckpointError::WriterNotCurrent)?;
        transaction.commit().await.map_err(storage_error)
    }

    fn decode_row(
        &self,
        row: &sqlx::postgres::PgRow,
    ) -> Result<Checkpoint, PostgresCheckpointError> {
        let checkpoint_id = row
            .try_get::<String, _>("checkpoint_id")
            .map_err(storage_error)?;
        let thread_id = row
            .try_get::<String, _>("thread_id")
            .map_err(storage_error)?;
        let state = decode_json::<State>(row, "state")?;
        let step = row.try_get::<i64, _>("step").map_err(storage_error)?;
        let step =
            usize::try_from(step).map_err(|_| PostgresCheckpointError::CorruptStoredState)?;
        let pending_nodes = decode_json::<Vec<String>>(row, "pending_nodes")?;
        let metadata = decode_json::<HashMap<String, Value>>(row, "metadata")?;
        let database_created_at = row
            .try_get::<DateTime<Utc>, _>("created_at")
            .map_err(storage_error)?;
        let created_at_rfc3339 = row
            .try_get::<String, _>("created_at_rfc3339")
            .map_err(storage_error)?;
        let created_at = DateTime::parse_from_rfc3339(&created_at_rfc3339)
            .map_err(|_| PostgresCheckpointError::CorruptStoredState)?
            .with_timezone(&Utc);
        if database_created_at.timestamp_micros() != created_at.timestamp_micros()
            || created_at.to_rfc3339_opts(SecondsFormat::AutoSi, true) != created_at_rfc3339
        {
            return Err(PostgresCheckpointError::CorruptStoredState);
        }
        let cleared_interrupt = row
            .try_get::<Option<String>, _>("cleared_interrupt")
            .map_err(storage_error)?;
        let attempts = decode_json::<HashMap<String, u32>>(row, "attempts")?;
        let child_ledger = decode_json::<HashMap<String, Value>>(row, "child_ledger")?;
        let checkpoint = Checkpoint {
            thread_id,
            checkpoint_id,
            state,
            step,
            pending_nodes,
            metadata,
            created_at,
            cleared_interrupt,
            attempts,
            child_ledger,
        };
        self.scope.require_thread(&checkpoint.thread_id)?;
        SerializedCheckpoint::new(&checkpoint, self.limits)
            .map_err(|_| PostgresCheckpointError::CorruptStoredState)?;
        Ok(checkpoint)
    }
}

#[async_trait]
impl Checkpointer for PostgresCheckpointer {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let span = checkpoint_operation_span("save");
        let result = self
            .save_checkpoint(checkpoint)
            .instrument(span.clone())
            .await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }

    async fn load(&self, thread_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.scope.require_thread(thread_id)?;
        let span = checkpoint_operation_span("load");
        let result = self.load_checkpoint(None).instrument(span.clone()).await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }

    async fn load_by_id(&self, checkpoint_id: &str) -> Result<Option<Checkpoint>, GraphError> {
        let span = checkpoint_operation_span("load_by_id");
        let result = self
            .load_checkpoint(Some(checkpoint_id))
            .instrument(span.clone())
            .await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }

    async fn list(&self, thread_id: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.scope.require_thread(thread_id)?;
        let span = checkpoint_operation_span("list");
        let result = self.list_checkpoints().instrument(span.clone()).await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }

    async fn delete(&self, thread_id: &str) -> Result<(), GraphError> {
        self.scope.require_thread(thread_id)?;
        let span = checkpoint_operation_span("delete");
        let result = self.delete_checkpoints().instrument(span.clone()).await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }

    async fn prune(&self, thread_id: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.scope.require_thread(thread_id)?;
        let span = checkpoint_operation_span("prune");
        let result = self
            .prune_checkpoints(policy)
            .instrument(span.clone())
            .await;
        record_checkpoint_result(&span, &result);
        result.map_err(Into::into)
    }
}

fn checkpoint_operation_span(operation: &'static str) -> tracing::Span {
    tracing::info_span!(
        "agent.checkpoint.persist",
        backend = "postgres",
        operation,
        outcome = tracing::field::Empty,
        error_code = tracing::field::Empty,
        retryable = tracing::field::Empty,
    )
}

fn record_checkpoint_result<T>(span: &tracing::Span, result: &Result<T, PostgresCheckpointError>) {
    match result {
        Ok(_) => {
            span.record("outcome", "succeeded");
        }
        Err(error) => {
            span.record("outcome", "failed");
            span.record("error_code", error.code());
            span.record("retryable", error.retryable());
        }
    }
}

enum WriterLock {
    Shared,
    Exclusive,
}

const CURRENT_WRITER_SHARED_SQL: &str = r"
SELECT 1
FROM elitea_runtime.agent_graph_checkpoint_writers AS writer
WHERE writer.tenant_id = $1
  AND writer.resource_project_id = $2
  AND writer.projection_project_id = $3
  AND writer.capability_id = $4
  AND writer.checkpoint_family = $5
  AND writer.definition_digest = $6
  AND writer.thread_id = $7
  AND writer.writer_claim_id = $8
  AND writer.writer_execution_id = $9
  AND writer.writer_generation = $10
  AND writer.writer_claim_attempt = $11
  AND writer.writer_lease_epoch = $12
FOR SHARE OF writer
";

const CURRENT_WRITER_EXCLUSIVE_SQL: &str = r"
SELECT 1
FROM elitea_runtime.agent_graph_checkpoint_writers AS writer
WHERE writer.tenant_id = $1
  AND writer.resource_project_id = $2
  AND writer.projection_project_id = $3
  AND writer.capability_id = $4
  AND writer.checkpoint_family = $5
  AND writer.definition_digest = $6
  AND writer.thread_id = $7
  AND writer.writer_claim_id = $8
  AND writer.writer_execution_id = $9
  AND writer.writer_generation = $10
  AND writer.writer_claim_attempt = $11
  AND writer.writer_lease_epoch = $12
FOR UPDATE OF writer
";

struct SerializedCheckpoint {
    state: String,
    step: i64,
    pending_nodes: String,
    metadata: String,
    created_at_rfc3339: String,
    attempts: String,
    child_ledger: String,
    total_bytes: usize,
}

impl SerializedCheckpoint {
    fn new(
        checkpoint: &Checkpoint,
        limits: CheckpointLimits,
    ) -> Result<Self, PostgresCheckpointError> {
        if !bounded_identity(&checkpoint.thread_id, MAX_THREAD_BYTES)
            || !bounded_identity(&checkpoint.checkpoint_id, MAX_IDENTITY_BYTES)
            || checkpoint
                .cleared_interrupt
                .as_deref()
                .is_some_and(|node| !bounded_identity(node, MAX_NODE_OR_KEY_BYTES))
        {
            return Err(PostgresCheckpointError::InvalidScope(
                "the checkpoint thread, ID or interrupt marker is malformed",
            ));
        }
        if checkpoint.pending_nodes.len() > limits.max_pending_nodes
            || checkpoint.state.len() > limits.max_map_entries
            || checkpoint.metadata.len() > limits.max_map_entries
            || checkpoint.attempts.len() > limits.max_map_entries
            || checkpoint.child_ledger.len() > limits.max_map_entries
        {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the checkpoint collection count exceeds its configured limit",
            ));
        }
        if checkpoint
            .pending_nodes
            .iter()
            .chain(checkpoint.state.keys())
            .chain(checkpoint.metadata.keys())
            .chain(checkpoint.attempts.keys())
            .chain(checkpoint.child_ledger.keys())
            .any(|value| !bounded_identity(value, MAX_NODE_OR_KEY_BYTES))
        {
            return Err(PostgresCheckpointError::InvalidScope(
                "a checkpoint node or map key is malformed",
            ));
        }
        validate_json_values(
            checkpoint
                .state
                .values()
                .chain(checkpoint.metadata.values())
                .chain(checkpoint.child_ledger.values()),
            limits,
        )?;
        let step = i64::try_from(checkpoint.step).map_err(|_| {
            PostgresCheckpointError::ResourceExhausted(
                "the checkpoint step is outside PostgreSQL BIGINT",
            )
        })?;
        let state = serialize_map_json(&checkpoint.state, limits.max_serialized_bytes)?;
        let mut total = state.len();
        let pending_nodes = serialize_json(
            &checkpoint.pending_nodes,
            limits.max_serialized_bytes - total,
        )?;
        total += pending_nodes.len();
        let metadata =
            serialize_map_json(&checkpoint.metadata, limits.max_serialized_bytes - total)?;
        total += metadata.len();
        let created_at_rfc3339 = checkpoint
            .created_at
            .to_rfc3339_opts(SecondsFormat::AutoSi, true);
        let attempts =
            serialize_map_json(&checkpoint.attempts, limits.max_serialized_bytes - total)?;
        total += attempts.len();
        let child_ledger = serialize_map_json(
            &checkpoint.child_ledger,
            limits.max_serialized_bytes - total,
        )?;
        total += child_ledger.len();
        Ok(Self {
            state,
            step,
            pending_nodes,
            metadata,
            created_at_rfc3339,
            attempts,
            child_ledger,
            total_bytes: total,
        })
    }
}

fn validate_json_values<'a>(
    values: impl IntoIterator<Item = &'a Value>,
    limits: CheckpointLimits,
) -> Result<(), PostgresCheckpointError> {
    let mut stack = values
        .into_iter()
        .map(|value| (value, 1usize))
        .collect::<Vec<_>>();
    let mut nodes = 0usize;
    while let Some((value, depth)) = stack.pop() {
        nodes = nodes
            .checked_add(1)
            .ok_or(PostgresCheckpointError::ResourceExhausted(
                "the checkpoint JSON node count overflowed",
            ))?;
        if nodes > limits.max_json_nodes || depth > limits.max_json_depth {
            return Err(PostgresCheckpointError::ResourceExhausted(
                "the checkpoint JSON shape exceeds its configured limit",
            ));
        }
        match value {
            Value::Array(values) => {
                if values.len() > limits.max_json_nodes.saturating_sub(nodes) {
                    return Err(PostgresCheckpointError::ResourceExhausted(
                        "the checkpoint JSON node count exceeds its configured limit",
                    ));
                }
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                if values.len() > limits.max_json_nodes.saturating_sub(nodes) {
                    return Err(PostgresCheckpointError::ResourceExhausted(
                        "the checkpoint JSON node count exceeds its configured limit",
                    ));
                }
                if values
                    .keys()
                    .any(|key| !bounded_identity(key, MAX_NODE_OR_KEY_BYTES))
                {
                    return Err(PostgresCheckpointError::InvalidScope(
                        "a nested checkpoint JSON key is malformed",
                    ));
                }
                stack.extend(values.values().map(|value| (value, depth + 1)));
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

fn serialize_map_json<T: serde::Serialize>(
    value: &HashMap<String, T>,
    maximum: usize,
) -> Result<String, PostgresCheckpointError> {
    let ordered = value
        .iter()
        .map(|(key, value)| (key.as_str(), value))
        .collect::<BTreeMap<_, _>>();
    serialize_json(&ordered, maximum)
}

fn serialize_json<T: serde::Serialize>(
    value: &T,
    maximum: usize,
) -> Result<String, PostgresCheckpointError> {
    let mut writer = CappedJsonWriter::new(maximum);
    if serde_json::to_writer(&mut writer, value).is_err() {
        return if writer.exhausted {
            Err(PostgresCheckpointError::ResourceExhausted(
                "the checkpoint serialized size exceeds its configured limit",
            ))
        } else {
            Err(PostgresCheckpointError::CorruptStoredState)
        };
    }
    String::from_utf8(writer.buffer).map_err(|_| PostgresCheckpointError::CorruptStoredState)
}

fn decode_json<T: serde::de::DeserializeOwned>(
    row: &sqlx::postgres::PgRow,
    column: &'static str,
) -> Result<T, PostgresCheckpointError> {
    let value = row.try_get::<String, _>(column).map_err(storage_error)?;
    serde_json::from_str(&value).map_err(|_| PostgresCheckpointError::CorruptStoredState)
}

struct CappedJsonWriter {
    buffer: Vec<u8>,
    maximum: usize,
    exhausted: bool,
}

impl CappedJsonWriter {
    const fn new(maximum: usize) -> Self {
        Self {
            buffer: Vec::new(),
            maximum,
            exhausted: false,
        }
    }
}

impl Write for CappedJsonWriter {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.maximum.saturating_sub(self.buffer.len()) {
            self.exhausted = true;
            return Err(io::Error::other(
                "checkpoint JSON exceeds its configured limit",
            ));
        }
        let required = self.buffer.len() + bytes.len();
        if required > self.buffer.capacity() {
            let desired = required
                .checked_next_power_of_two()
                .unwrap_or(self.maximum)
                .max(1_024)
                .min(self.maximum);
            if self
                .buffer
                .try_reserve_exact(desired - self.buffer.len())
                .is_err()
            {
                self.exhausted = true;
                return Err(io::Error::other("checkpoint JSON allocation failed"));
            }
        }
        self.buffer.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn bounded_identity(value: &str, maximum: usize) -> bool {
    !value.is_empty()
        && value.len() <= maximum
        && !value
            .as_bytes()
            .iter()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}

fn duration_micros_ceil(duration: std::time::Duration) -> Result<i64, PostgresCheckpointError> {
    let micros = duration
        .as_micros()
        .checked_add(u128::from(!duration.subsec_nanos().is_multiple_of(1_000)))
        .ok_or(PostgresCheckpointError::InvalidConfiguration(
            "the checkpoint retention age is outside PostgreSQL interval bounds",
        ))?;
    i64::try_from(micros).map_err(|_| {
        PostgresCheckpointError::InvalidConfiguration(
            "the checkpoint retention age is outside PostgreSQL interval bounds",
        )
    })
}

fn storage_error(source: sqlx::Error) -> PostgresCheckpointError {
    if retryable_storage_error(&source) {
        PostgresCheckpointError::StorageUnavailable { source }
    } else {
        PostgresCheckpointError::StorageFailure { source }
    }
}

fn retryable_storage_error(error: &sqlx::Error) -> bool {
    match error {
        sqlx::Error::Io(_)
        | sqlx::Error::Tls(_)
        | sqlx::Error::PoolTimedOut
        | sqlx::Error::PoolClosed
        | sqlx::Error::WorkerCrashed
        | sqlx::Error::BeginFailed => true,
        sqlx::Error::Database(database) => database.code().is_some_and(|code| {
            code.starts_with("08")
                || matches!(
                    code.as_ref(),
                    "40001" | "40P01" | "55P03" | "57014" | "57P01" | "57P02" | "57P03"
                )
        }),
        _ => false,
    }
}

impl fmt::Debug for PostgresCheckpointer {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PostgresCheckpointer")
            .field("checkpoint_family", &CHECKPOINT_FAMILY)
            .field("limits", &self.limits)
            .finish_non_exhaustive()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn authority(thread_id: &str) -> CheckpointWriterAuthority {
        CheckpointWriterAuthority::new(
            "tenant-1".to_owned(),
            1,
            1,
            APPLICATION_CAPABILITY_ID,
            [0x42; 32],
            thread_id.to_owned(),
            "execution-1".to_owned(),
            2,
            "claim-1".to_owned(),
            3,
            4,
            1_700_000_000_123_456,
            "session-1".to_owned(),
            "producer-1".to_owned(),
            [0x66; 32],
        )
        .expect("valid authority")
    }

    fn complete_checkpoint() -> Checkpoint {
        let mut checkpoint = Checkpoint::new(
            "thread-1",
            HashMap::from([
                (
                    "messages".to_owned(),
                    json!([{"role": "user", "content": "hello"}]),
                ),
                ("counter".to_owned(), json!(7)),
            ]),
            9,
            vec!["review".to_owned(), "publish".to_owned()],
        );
        checkpoint.checkpoint_id = "checkpoint-1".to_owned();
        checkpoint
            .metadata
            .insert("route".to_owned(), json!("safe"));
        checkpoint.cleared_interrupt = Some("approval".to_owned());
        checkpoint.attempts.insert("review".to_owned(), 2);
        checkpoint
            .child_ledger
            .insert("child/one".to_owned(), json!({"status": "completed"}));
        checkpoint
    }

    #[test]
    fn complete_adk_checkpoint_shape_is_bounded_without_loss() {
        let checkpoint = complete_checkpoint();

        let serialized = SerializedCheckpoint::new(&checkpoint, CheckpointLimits::default())
            .expect("complete checkpoint is accepted");

        assert_eq!(serialized.step, 9);
        assert_eq!(
            serde_json::from_str::<State>(&serialized.state).expect("state JSON"),
            checkpoint.state
        );
        assert_eq!(
            serde_json::from_str::<Vec<String>>(&serialized.pending_nodes).expect("pending JSON"),
            checkpoint.pending_nodes
        );
        assert_eq!(
            serde_json::from_str::<HashMap<String, u32>>(&serialized.attempts)
                .expect("attempt JSON"),
            checkpoint.attempts
        );
        assert_eq!(
            serde_json::from_str::<HashMap<String, Value>>(&serialized.child_ledger)
                .expect("child ledger JSON"),
            checkpoint.child_ledger
        );
    }

    #[test]
    fn authority_and_thread_scope_reject_malformed_or_cross_thread_values() {
        assert!(
            CheckpointWriterAuthority::new(
                "tenant-1".to_owned(),
                1,
                1,
                APPLICATION_CAPABILITY_ID,
                [0x42; 32],
                "thread-1".to_owned(),
                "execution-1".to_owned(),
                1,
                "claim-1".to_owned(),
                1,
                1,
                1_700_000_000_123_456,
                "session-1".to_owned(),
                "producer-1".to_owned(),
                [0x66; 32],
            )
            .is_ok()
        );
        assert!(
            CheckpointWriterAuthority::new(
                "tenant-1".to_owned(),
                0,
                1,
                APPLICATION_CAPABILITY_ID,
                [0x42; 32],
                "thread-1".to_owned(),
                "execution-1".to_owned(),
                1,
                "claim-1".to_owned(),
                1,
                1,
                1_700_000_000_123_456,
                "session-1".to_owned(),
                "producer-1".to_owned(),
                [0x66; 32],
            )
            .is_err()
        );
        assert!(
            CheckpointWriterAuthority::new(
                "tenant-1".to_owned(),
                1,
                1,
                APPLICATION_CAPABILITY_ID,
                [0x42; 32],
                "wrong\nthread".to_owned(),
                "execution-1".to_owned(),
                1,
                "claim-1".to_owned(),
                1,
                1,
                1_700_000_000_123_456,
                "session-1".to_owned(),
                "producer-1".to_owned(),
                [0x66; 32],
            )
            .is_err()
        );

        let scope = CheckpointScope {
            authority: authority("thread-1"),
        };
        assert!(scope.require_thread("thread-1").is_ok());
        assert!(matches!(
            scope.require_thread("thread-2"),
            Err(PostgresCheckpointError::InvalidScope(_))
        ));
    }

    #[test]
    fn bounds_reject_oversized_frontiers_maps_and_payloads() {
        let mut checkpoint = complete_checkpoint();
        checkpoint.pending_nodes.push("extra".to_owned());
        let limits = CheckpointLimits {
            max_pending_nodes: 2,
            ..CheckpointLimits::default()
        };
        assert!(matches!(
            SerializedCheckpoint::new(&checkpoint, limits),
            Err(PostgresCheckpointError::ResourceExhausted(_))
        ));

        let mut checkpoint = complete_checkpoint();
        checkpoint.state.insert("extra".to_owned(), json!(true));
        let limits = CheckpointLimits {
            max_map_entries: 2,
            ..CheckpointLimits::default()
        };
        assert!(matches!(
            SerializedCheckpoint::new(&checkpoint, limits),
            Err(PostgresCheckpointError::ResourceExhausted(_))
        ));

        let checkpoint = complete_checkpoint();
        let limits = CheckpointLimits {
            max_serialized_bytes: 5,
            ..CheckpointLimits::default()
        };
        assert!(matches!(
            SerializedCheckpoint::new(&checkpoint, limits),
            Err(PostgresCheckpointError::ResourceExhausted(_))
        ));
    }

    #[test]
    fn capped_canonical_json_accepts_exact_limit_and_rejects_limit_plus_one() {
        let mut checkpoint = complete_checkpoint();
        checkpoint.state = HashMap::from([
            ("z-last".to_owned(), json!(true)),
            (
                "escaped".to_owned(),
                Value::String("\"\\\n".repeat(32 * 1024)),
            ),
            ("a-first".to_owned(), json!(false)),
        ]);
        let baseline = SerializedCheckpoint::new(&checkpoint, CheckpointLimits::default())
            .expect("large escaped checkpoint fits the database profile");
        let exact_limits = CheckpointLimits {
            max_serialized_bytes: baseline.total_bytes,
            ..CheckpointLimits::default()
        };
        assert!(SerializedCheckpoint::new(&checkpoint, exact_limits).is_ok());
        let too_small = CheckpointLimits {
            max_serialized_bytes: baseline.total_bytes - 1,
            ..CheckpointLimits::default()
        };
        assert!(matches!(
            SerializedCheckpoint::new(&checkpoint, too_small),
            Err(PostgresCheckpointError::ResourceExhausted(_))
        ));

        let reverse = HashMap::from([
            ("a-first".to_owned(), json!(false)),
            (
                "escaped".to_owned(),
                Value::String("\"\\\n".repeat(32 * 1024)),
            ),
            ("z-last".to_owned(), json!(true)),
        ]);
        assert_eq!(
            serialize_map_json(&checkpoint.state, MAX_DATABASE_PAYLOAD_BYTES)
                .expect("canonical first map"),
            serialize_map_json(&reverse, MAX_DATABASE_PAYLOAD_BYTES)
                .expect("canonical reverse map")
        );
    }

    #[test]
    fn errors_are_stable_descriptive_and_data_free() {
        let error = PostgresCheckpointError::WriterNotCurrent;
        assert_eq!(error.code(), "checkpoint.writer_not_current");
        assert!(!error.retryable());
        assert_eq!(
            error.to_string(),
            "the PostgreSQL checkpoint writer is no longer current"
        );

        let graph_error: GraphError = error.into();
        assert_eq!(
            graph_error.to_string(),
            concat!(
                "Checkpoint error: checkpoint.writer_not_current: ",
                "the PostgreSQL checkpoint writer is no longer current"
            )
        );

        let unavailable = storage_error(sqlx::Error::PoolTimedOut);
        assert_eq!(unavailable.code(), "checkpoint.storage_unavailable");
        assert!(unavailable.retryable());
        assert_eq!(
            unavailable.to_string(),
            "PostgreSQL checkpoint storage is temporarily unavailable"
        );

        let permanent = storage_error(sqlx::Error::RowNotFound);
        assert_eq!(permanent.code(), "checkpoint.storage_failure");
        assert!(!permanent.retryable());
        assert_eq!(
            permanent.to_string(),
            "the PostgreSQL checkpoint operation failed"
        );
    }

    #[test]
    fn retention_age_is_rounded_to_postgresql_precision_and_bounded() {
        assert_eq!(
            duration_micros_ceil(std::time::Duration::from_nanos(1))
                .expect("one nanosecond is representable"),
            1
        );
        assert_eq!(
            duration_micros_ceil(std::time::Duration::from_micros(7))
                .expect("whole microseconds are preserved"),
            7
        );
    }
}
