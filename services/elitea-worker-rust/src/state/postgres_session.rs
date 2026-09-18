//! Claim-bound ADK session persistence in Elitea's `PostgreSQL` runtime schema.
//!
//! This is intentionally separate from graph checkpoints. `SessionService`
//! owns conversation events and ADK state for every Runner; `Checkpointer`
//! additionally owns workflow frontier and interrupt state for graph agents.

#![allow(dead_code)] // Production assembly remains capability-gated.

use std::collections::{BTreeMap, HashMap};
use std::fmt;
use std::sync::Arc;

use adk_rust::session::{
    AppendEventRequest, CreateRequest, DeleteRequest, Events, GetRequest, ListRequest, Session,
    SessionService, State, extract_state_deltas, merge_states,
};
use adk_rust::{AdkError, AdkIdentity, ErrorCategory, ErrorComponent, Event};
use async_trait::async_trait;
use chrono::{DateTime, TimeZone as _, Utc};
use serde::de::DeserializeOwned;
use serde_json::Value;
use sqlx::{PgPool, Postgres, Row, Transaction};
use thiserror::Error;
use tracing::Instrument as _;
use zeroize::Zeroizing;

use super::StateWriterLease;

const SESSION_FAMILY: &str = "adk-session.2.0.0.v1";
const MAX_SHARED_STATE_BYTES: usize = 1024 * 1024;
const MAX_STATE_BYTES: usize = 8 * 1024 * 1024;
const MAX_EVENT_BYTES: usize = 16 * 1024 * 1024;
const MAX_EVENTS: usize = 4096;
const MAX_RETAINED_EVENT_BYTES: usize = 64 * 1024 * 1024;
const MAX_JSON_DEPTH: usize = 64;
const MAX_JSON_NODES: usize = 65_536;
const MAX_TENANT_BYTES: usize = 256;
const MAX_THREAD_BYTES: usize = 512;
const MAX_IDENTITY_BYTES: usize = 256;
pub(super) const APPLICATION_CAPABILITY_ID: &str = "agent.execute.application.v1";
const ADHOC_CAPABILITY_ID: &str = "agent.execute.adhoc.v1";

/// Runtime bounds which can only tighten the database contract.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SessionLimits {
    pub max_state_bytes: usize,
    pub max_event_bytes: usize,
    pub max_events: usize,
    pub max_retained_event_bytes: usize,
    pub max_json_depth: usize,
    pub max_json_nodes: usize,
}

impl Default for SessionLimits {
    fn default() -> Self {
        Self {
            max_state_bytes: MAX_STATE_BYTES,
            max_event_bytes: MAX_EVENT_BYTES,
            max_events: MAX_EVENTS,
            max_retained_event_bytes: MAX_RETAINED_EVENT_BYTES,
            max_json_depth: MAX_JSON_DEPTH,
            max_json_nodes: MAX_JSON_NODES,
        }
    }
}

impl SessionLimits {
    fn validate(self) -> Result<Self, PostgresSessionError> {
        if self.max_state_bytes == 0
            || self.max_state_bytes > MAX_STATE_BYTES
            || self.max_event_bytes == 0
            || self.max_event_bytes > MAX_EVENT_BYTES
            || self.max_events == 0
            || self.max_events > MAX_EVENTS
            || self.max_retained_event_bytes == 0
            || self.max_retained_event_bytes > MAX_RETAINED_EVENT_BYTES
            || self.max_json_depth == 0
            || self.max_json_depth > MAX_JSON_DEPTH
            || self.max_json_nodes == 0
            || self.max_json_nodes > MAX_JSON_NODES
        {
            return Err(PostgresSessionError::InvalidConfiguration);
        }
        Ok(self)
    }
}

/// Stable, data-free failure at the Elitea session boundary.
#[derive(Debug, Error)]
pub enum PostgresSessionError {
    #[error("the PostgreSQL session configuration is invalid")]
    InvalidConfiguration,
    #[error("the PostgreSQL session identity is outside the authorized scope")]
    InvalidScope,
    #[error("the PostgreSQL session exceeds an approved resource limit")]
    ResourceExhausted,
    #[error("the PostgreSQL session writer is no longer current")]
    WriterNotCurrent,
    #[error("the PostgreSQL session event conflicts with different durable content")]
    EventConflict,
    #[error("the PostgreSQL session row is malformed or incompatible")]
    CorruptStoredState,
    #[error("PostgreSQL session storage is temporarily unavailable")]
    StorageUnavailable {
        #[source]
        source: sqlx::Error,
    },
    #[error("the PostgreSQL session operation failed")]
    StorageFailure {
        #[source]
        source: sqlx::Error,
    },
}

impl PostgresSessionError {
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "session.invalid_configuration",
            Self::InvalidScope => "session.invalid_scope",
            Self::ResourceExhausted => "session.resource_exhausted",
            Self::WriterNotCurrent => "session.writer_not_current",
            Self::EventConflict => "session.event_conflict",
            Self::CorruptStoredState => "session.corrupt_state",
            Self::StorageUnavailable { .. } => "session.storage_unavailable",
            Self::StorageFailure { .. } => "session.storage_failure",
        }
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, message) = match self {
            Self::InvalidConfiguration | Self::InvalidScope => (
                ErrorCategory::InvalidInput,
                "the durable session identity or configuration is invalid",
            ),
            Self::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "the durable session reached an approved resource limit",
            ),
            Self::WriterNotCurrent => (
                ErrorCategory::Cancelled,
                "the durable session writer is no longer current",
            ),
            Self::EventConflict | Self::CorruptStoredState => (
                ErrorCategory::Internal,
                "the durable session state is inconsistent",
            ),
            Self::StorageUnavailable { .. } => (
                ErrorCategory::Unavailable,
                "durable session storage is temporarily unavailable",
            ),
            Self::StorageFailure { .. } => (
                ErrorCategory::Internal,
                "the durable session operation failed",
            ),
        };
        AdkError::new(ErrorComponent::Session, category, self.code(), message).with_source(self)
    }
}

/// One non-cloneable claim/fence binding for an exact ADK session lineage.
///
/// Raw construction is crate-private. The native invocation coordinator will
/// derive it from the accepted claim and the admitted frozen definition.
#[derive(Clone)]
pub(crate) struct SessionWriterAuthority {
    tenant_id: String,
    resource_project_id: i32,
    projection_project_id: i32,
    capability_id: &'static str,
    definition_digest: [u8; 32],
    thread_id: String,
    app_name: String,
    user_id: String,
    session_id: String,
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

impl SessionWriterAuthority {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn new(
        tenant_id: String,
        resource_project_id: i32,
        projection_project_id: i32,
        capability_id: &'static str,
        definition_digest: [u8; 32],
        thread_id: String,
        app_name: String,
        user_id: String,
        session_id: String,
        execution_id: String,
        generation: u64,
        claim_id: String,
        claim_attempt: u64,
        lease_epoch: u64,
        claim_started_at_unix_micros: i64,
        workload_session_id: String,
        producer_id: String,
        fence_token: [u8; 32],
    ) -> Result<Self, PostgresSessionError> {
        let generation =
            i64::try_from(generation).map_err(|_| PostgresSessionError::InvalidScope)?;
        let claim_attempt =
            i64::try_from(claim_attempt).map_err(|_| PostgresSessionError::InvalidScope)?;
        let lease_epoch =
            i64::try_from(lease_epoch).map_err(|_| PostgresSessionError::InvalidScope)?;
        let claim_started_at = Utc
            .timestamp_micros(claim_started_at_unix_micros)
            .single()
            .ok_or(PostgresSessionError::InvalidScope)?;
        let authority = Self {
            tenant_id,
            resource_project_id,
            projection_project_id,
            capability_id,
            definition_digest,
            thread_id,
            app_name,
            user_id,
            session_id,
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

    fn validate(&self) -> Result<(), PostgresSessionError> {
        if !bounded(&self.tenant_id, MAX_TENANT_BYTES)
            || !bounded(&self.thread_id, MAX_THREAD_BYTES)
            || !bounded(&self.app_name, MAX_IDENTITY_BYTES)
            || !bounded(&self.user_id, MAX_IDENTITY_BYTES)
            || !bounded(&self.session_id, MAX_IDENTITY_BYTES)
            || !bounded(&self.execution_id, MAX_IDENTITY_BYTES)
            || !bounded(&self.claim_id, MAX_IDENTITY_BYTES)
            || !bounded(&self.workload_session_id, MAX_IDENTITY_BYTES)
            || !bounded(&self.producer_id, MAX_IDENTITY_BYTES)
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
            return Err(PostgresSessionError::InvalidScope);
        }
        Ok(())
    }
}

/// ADK `SessionService` activated for one supervised Elitea execution claim.
///
/// Main-authored claim time orders durable writer takeover in `agentstate`;
/// the attached live-lease guard rejects local work after cancellation,
/// supervision loss, or expiry. This deliberately avoids querying Main's
/// business tables from the separate state database.
pub struct PostgresSessionService {
    pool: PgPool,
    authority: SessionWriterAuthority,
    limits: SessionLimits,
    state_writer_lease: Arc<dyn StateWriterLease>,
    parent: Option<Arc<Self>>,
}

struct StoredSession {
    app_name: String,
    user_id: String,
    session_id: String,
    state: HashMap<String, Value>,
    events: Vec<Event>,
    updated_at: DateTime<Utc>,
}

impl Session for StoredSession {
    fn id(&self) -> &str {
        &self.session_id
    }

    fn app_name(&self) -> &str {
        &self.app_name
    }

    fn user_id(&self) -> &str {
        &self.user_id
    }

    fn state(&self) -> &dyn State {
        self
    }

    fn events(&self) -> &dyn Events {
        self
    }

    fn last_update_time(&self) -> DateTime<Utc> {
        self.updated_at
    }
}

impl State for StoredSession {
    fn get(&self, key: &str) -> Option<Value> {
        self.state.get(key).cloned()
    }

    fn set(&mut self, key: String, value: Value) {
        if adk_rust::validate_state_key(&key).is_ok() {
            self.state.insert(key, value);
        }
    }

    fn all(&self) -> HashMap<String, Value> {
        self.state.clone()
    }
}

impl Events for StoredSession {
    fn all(&self) -> Vec<Event> {
        self.events.clone()
    }

    fn len(&self) -> usize {
        self.events.len()
    }

    fn at(&self, index: usize) -> Option<&Event> {
        self.events.get(index)
    }
}

impl fmt::Debug for PostgresSessionService {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PostgresSessionService")
            .field("session_family", &SESSION_FAMILY)
            .field("limits", &self.limits)
            .finish_non_exhaustive()
    }
}

impl PostgresSessionService {
    /// Activate this claim as the only current writer for the exact session.
    pub(crate) async fn activate(
        pool: PgPool,
        authority: SessionWriterAuthority,
        limits: SessionLimits,
        state_writer_lease: Arc<dyn StateWriterLease>,
    ) -> Result<Self, PostgresSessionError> {
        Self::activate_with_parent(pool, authority, limits, state_writer_lease, None).await
    }

    async fn activate_with_parent(
        pool: PgPool,
        authority: SessionWriterAuthority,
        limits: SessionLimits,
        state_writer_lease: Arc<dyn StateWriterLease>,
        parent: Option<Arc<Self>>,
    ) -> Result<Self, PostgresSessionError> {
        authority.validate()?;
        let limits = limits.validate()?;
        state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresSessionError::WriterNotCurrent)?;
        let mut transaction = pool.begin().await.map_err(storage_error)?;
        if let Some(parent) = &parent {
            parent.lock_writer_row(&mut transaction, false).await?;
        }
        let activated =
            activate_writer_row(&mut transaction, &authority, authority.claim_started_at).await?;
        if activated.as_deref() != Some(authority.claim_id.as_str()) {
            return Err(PostgresSessionError::WriterNotCurrent);
        }
        state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresSessionError::WriterNotCurrent)?;
        transaction.commit().await.map_err(storage_error)?;
        Ok(Self {
            pool,
            authority,
            limits,
            state_writer_lease,
            parent,
        })
    }

    /// Derive a worker-owned model session without widening the root service scope.
    /// Both writer rows remain fenced by the same supervised execution claim.
    pub(crate) async fn model_scope(
        self: &Arc<Self>,
        identity: &AdkIdentity,
    ) -> Result<Self, PostgresSessionError> {
        self.require_app_user(identity.app_name.as_ref(), identity.user_id.as_ref())?;
        let suffix = identity.session_id.as_ref().strip_prefix("elitea-model-");
        if self.parent.is_some()
            || !suffix.is_some_and(|value| {
                value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
            })
        {
            return Err(PostgresSessionError::InvalidScope);
        }
        let mut authority = self.authority.clone();
        authority.session_id = identity.session_id.to_string();
        Self::activate_with_parent(
            self.pool.clone(),
            authority,
            self.limits,
            self.state_writer_lease.clone(),
            Some(self.clone()),
        )
        .await
    }

    fn require_identity(
        &self,
        app_name: &str,
        user_id: &str,
        session_id: &str,
    ) -> Result<(), PostgresSessionError> {
        if app_name != self.authority.app_name
            || user_id != self.authority.user_id
            || session_id != self.authority.session_id
        {
            return Err(PostgresSessionError::InvalidScope);
        }
        Ok(())
    }

    fn require_app_user(&self, app_name: &str, user_id: &str) -> Result<(), PostgresSessionError> {
        if app_name != self.authority.app_name || user_id != self.authority.user_id {
            return Err(PostgresSessionError::InvalidScope);
        }
        Ok(())
    }

    async fn lock_current_writer(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        exclusive: bool,
    ) -> Result<(), PostgresSessionError> {
        // Lock the root first. Replacement cannot admit a stale child write,
        // even before this worker observes loss of its live lease.
        if let Some(parent) = &self.parent {
            parent.lock_writer_row(transaction, false).await?;
        }
        self.lock_writer_row(transaction, exclusive).await
    }

    async fn lock_writer_row(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        exclusive: bool,
    ) -> Result<(), PostgresSessionError> {
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresSessionError::WriterNotCurrent)?;
        let writer_sql = if exclusive {
            CURRENT_WRITER_EXCLUSIVE_SQL
        } else {
            CURRENT_WRITER_SHARED_SQL
        };
        sqlx::query_scalar::<_, i32>(writer_sql)
            .bind(&self.authority.tenant_id)
            .bind(self.authority.resource_project_id)
            .bind(self.authority.projection_project_id)
            .bind(self.authority.capability_id)
            .bind(SESSION_FAMILY)
            .bind(self.authority.definition_digest.as_slice())
            .bind(&self.authority.thread_id)
            .bind(&self.authority.app_name)
            .bind(&self.authority.user_id)
            .bind(&self.authority.session_id)
            .bind(&self.authority.claim_id)
            .bind(&self.authority.execution_id)
            .bind(self.authority.generation)
            .bind(self.authority.claim_attempt)
            .bind(self.authority.lease_epoch)
            .fetch_optional(&mut **transaction)
            .await
            .map_err(storage_error)?
            .ok_or(PostgresSessionError::WriterNotCurrent)?;
        Ok(())
    }

    async fn commit_current(
        &self,
        transaction: Transaction<'_, Postgres>,
    ) -> Result<(), PostgresSessionError> {
        self.state_writer_lease
            .ensure_current()
            .map_err(|_| PostgresSessionError::WriterNotCurrent)?;
        transaction.commit().await.map_err(storage_error)
    }

    async fn create_session(
        &self,
        req: CreateRequest,
    ) -> Result<Box<dyn Session>, PostgresSessionError> {
        req.try_identity()
            .map_err(|_| PostgresSessionError::InvalidScope)?;
        let session_id = req
            .session_id
            .as_deref()
            .ok_or(PostgresSessionError::InvalidScope)?
            .to_owned();
        self.require_identity(&req.app_name, &req.user_id, &session_id)?;
        let (app_state, user_state, session_state) = extract_state_deltas(&req.state);
        validate_state(&app_state, self.limits)?;
        validate_state(&user_state, self.limits)?;
        validate_state(&session_state, self.limits)?;
        let now = Utc::now();
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, true).await?;
        upsert_app_state(
            &mut transaction,
            &self.authority,
            &app_state,
            now,
            self.limits,
        )
        .await?;
        upsert_user_state(
            &mut transaction,
            &self.authority,
            &user_state,
            now,
            self.limits,
        )
        .await?;
        let stored_app_state =
            load_app_state(&mut transaction, &self.authority, self.limits).await?;
        let stored_user_state =
            load_user_state(&mut transaction, &self.authority, self.limits).await?;
        let session_json = encode_state(&session_state, self.limits)?;
        sqlx::query(
            r"
INSERT INTO elitea_runtime.agent_sessions (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, thread_id, app_name, user_id,
    session_id, state, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .bind(&session_json)
        .bind(now)
        .execute(&mut *transaction)
        .await
        .map_err(storage_error)?;
        self.commit_current(transaction).await?;
        let merged = merge_states(&stored_app_state, &stored_user_state, &session_state);
        Ok(Box::new(StoredSession {
            app_name: req.app_name,
            user_id: req.user_id,
            session_id,
            state: merged,
            events: Vec::new(),
            updated_at: now,
        }))
    }

    async fn get_session(&self, req: GetRequest) -> Result<Box<dyn Session>, PostgresSessionError> {
        req.try_identity()
            .map_err(|_| PostgresSessionError::InvalidScope)?;
        self.require_identity(&req.app_name, &req.user_id, &req.session_id)?;
        if req
            .num_recent_events
            .is_some_and(|limit| limit > self.limits.max_events)
        {
            return Err(PostgresSessionError::ResourceExhausted);
        }
        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, false).await?;
        let session_row = self.load_session_row(&mut transaction).await?;
        let events = self
            .load_events(&mut transaction, req.num_recent_events, req.after)
            .await?;
        let app_state = load_app_state(&mut transaction, &self.authority, self.limits).await?;
        let user_state = load_user_state(&mut transaction, &self.authority, self.limits).await?;
        self.commit_current(transaction).await?;
        let (session_state, updated_at) = session_row;
        Ok(Box::new(StoredSession {
            app_name: req.app_name,
            user_id: req.user_id,
            session_id: req.session_id,
            state: merge_states(&app_state, &user_state, &session_state),
            events,
            updated_at,
        }))
    }

    async fn load_session_row(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
    ) -> Result<(HashMap<String, Value>, DateTime<Utc>), PostgresSessionError> {
        let row = sqlx::query(
            r"
SELECT state, updated_at
FROM elitea_runtime.agent_sessions
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .fetch_optional(&mut **transaction)
        .await
        .map_err(storage_error)?
        .ok_or(PostgresSessionError::InvalidScope)?;
        let state = decode_state(
            &row.try_get::<String, _>("state").map_err(storage_error)?,
            self.limits,
        )?;
        let updated_at = row
            .try_get::<DateTime<Utc>, _>("updated_at")
            .map_err(storage_error)?;
        Ok((state, updated_at))
    }

    async fn load_events(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        num_recent_events: Option<usize>,
        after: Option<DateTime<Utc>>,
    ) -> Result<Vec<Event>, PostgresSessionError> {
        let requested = num_recent_events.unwrap_or(self.limits.max_events);
        let fetch_limit = requested
            .checked_add(usize::from(num_recent_events.is_none()))
            .ok_or(PostgresSessionError::ResourceExhausted)?;
        let fetch_limit =
            i64::try_from(fetch_limit).map_err(|_| PostgresSessionError::ResourceExhausted)?;
        let rows = sqlx::query(
            r"
SELECT event_payload, event_timestamp
FROM elitea_runtime.agent_session_events
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
ORDER BY event_ordinal DESC
LIMIT $11
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .bind(fetch_limit)
        .fetch_all(&mut **transaction)
        .await
        .map_err(storage_error)?;
        if num_recent_events.is_none() && rows.len() > self.limits.max_events {
            return Err(PostgresSessionError::ResourceExhausted);
        }
        let mut events = Vec::with_capacity(rows.len().min(requested));
        for row in rows.into_iter().take(requested).rev() {
            let payload = row
                .try_get::<String, _>("event_payload")
                .map_err(storage_error)?;
            let timestamp = row
                .try_get::<DateTime<Utc>, _>("event_timestamp")
                .map_err(storage_error)?;
            let event = decode_event(&payload, self.limits)?;
            // PostgreSQL TIMESTAMPTZ stores microseconds while the canonical
            // event payload retains ADK's full nanosecond timestamp. Compare
            // the relational audit column at PostgreSQL precision without
            // discarding the exact timestamp returned from the payload.
            if postgres_timestamp(event.timestamp)? != timestamp {
                return Err(PostgresSessionError::CorruptStoredState);
            }
            if after.is_none_or(|after| event.timestamp >= after) {
                events.push(event);
            }
        }
        Ok(events)
    }

    async fn append(
        &self,
        identity: &adk_rust::AdkIdentity,
        mut event: Event,
    ) -> Result<(), PostgresSessionError> {
        self.require_identity(
            identity.app_name.as_ref(),
            identity.user_id.as_ref(),
            identity.session_id.as_ref(),
        )?;
        if !bounded(&event.id, MAX_IDENTITY_BYTES)
            || !bounded(&event.invocation_id, MAX_IDENTITY_BYTES)
            || !bounded(&event.author, MAX_IDENTITY_BYTES)
            || event.branch.len() > MAX_IDENTITY_BYTES
        {
            return Err(PostgresSessionError::InvalidScope);
        }
        event
            .actions
            .state_delta
            .retain(|key, _| !key.starts_with(adk_rust::session::KEY_PREFIX_TEMP));
        let encoded_event = encode_event(&event, self.limits)?;
        let (app_delta, user_delta, session_delta) =
            extract_state_deltas(&event.actions.state_delta);
        validate_state(&app_delta, self.limits)?;
        validate_state(&user_delta, self.limits)?;
        validate_state(&session_delta, self.limits)?;

        let mut transaction = self.pool.begin().await.map_err(storage_error)?;
        self.lock_current_writer(&mut transaction, true).await?;
        let exact_existing = sqlx::query_scalar::<_, bool>(
            r"
SELECT event_payload = $11 AND event_timestamp = $12
FROM elitea_runtime.agent_session_events
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
  AND event_id = $13
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .bind(&encoded_event)
        .bind(event.timestamp)
        .bind(&event.id)
        .fetch_optional(&mut *transaction)
        .await
        .map_err(storage_error)?;
        match exact_existing {
            Some(true) => {
                self.commit_current(transaction).await?;
                return Ok(());
            }
            Some(false) => return Err(PostgresSessionError::EventConflict),
            None => {}
        }

        self.ensure_event_capacity(&mut transaction, encoded_event.len())
            .await?;
        let now = event.timestamp;
        upsert_app_state(
            &mut transaction,
            &self.authority,
            &app_delta,
            now,
            self.limits,
        )
        .await?;
        upsert_user_state(
            &mut transaction,
            &self.authority,
            &user_delta,
            now,
            self.limits,
        )
        .await?;
        self.merge_session_state(&mut transaction, &session_delta, now)
            .await?;
        let ordinal = self.allocate_event_ordinal(&mut transaction).await?;
        insert_event(
            &mut transaction,
            &self.authority,
            &event,
            &encoded_event,
            ordinal,
        )
        .await?;
        self.commit_current(transaction).await?;
        Ok(())
    }

    async fn ensure_event_capacity(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        candidate_bytes: usize,
    ) -> Result<(), PostgresSessionError> {
        let (count, bytes) = sqlx::query_as::<_, (i64, i64)>(
            r"
SELECT count(*), COALESCE(sum(payload_bytes), 0)::bigint
FROM elitea_runtime.agent_session_events
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .fetch_one(&mut **transaction)
        .await
        .map_err(storage_error)?;
        let count = usize::try_from(count).map_err(|_| PostgresSessionError::CorruptStoredState)?;
        let retained = usize::try_from(bytes)
            .ok()
            .and_then(|bytes| bytes.checked_add(candidate_bytes))
            .ok_or(PostgresSessionError::ResourceExhausted)?;
        if count >= self.limits.max_events || retained > self.limits.max_retained_event_bytes {
            return Err(PostgresSessionError::ResourceExhausted);
        }
        Ok(())
    }

    async fn allocate_event_ordinal(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
    ) -> Result<i64, PostgresSessionError> {
        sqlx::query_scalar::<_, i64>(
            r"
UPDATE elitea_runtime.agent_session_writers
SET next_event_ordinal = next_event_ordinal + 1
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
  AND next_event_ordinal < 9223372036854775807
RETURNING next_event_ordinal - 1
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .fetch_optional(&mut **transaction)
        .await
        .map_err(storage_error)?
        .ok_or(PostgresSessionError::ResourceExhausted)
    }

    async fn merge_session_state(
        &self,
        transaction: &mut Transaction<'_, Postgres>,
        delta: &HashMap<String, Value>,
        updated_at: DateTime<Utc>,
    ) -> Result<(), PostgresSessionError> {
        let row = sqlx::query(
            r"
SELECT state
FROM elitea_runtime.agent_sessions
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
FOR UPDATE
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .fetch_optional(&mut **transaction)
        .await
        .map_err(storage_error)?
        .ok_or(PostgresSessionError::InvalidScope)?;
        let mut state = decode_state(
            &row.try_get::<String, _>("state").map_err(storage_error)?,
            self.limits,
        )?;
        state.extend(delta.clone());
        let state = encode_state(&state, self.limits)?;
        sqlx::query(
            r"
UPDATE elitea_runtime.agent_sessions
SET state = $11, updated_at = $12
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .bind(&state)
        .bind(updated_at)
        .execute(&mut **transaction)
        .await
        .map_err(storage_error)?;
        Ok(())
    }
}

#[async_trait]
impl SessionService for PostgresSessionService {
    async fn create(&self, req: CreateRequest) -> adk_rust::Result<Box<dyn Session>> {
        let span = session_operation_span("create");
        let result = self.create_session(req).instrument(span.clone()).await;
        record_session_result(&span, &result);
        result.map_err(PostgresSessionError::into_adk)
    }

    async fn get(&self, req: GetRequest) -> adk_rust::Result<Box<dyn Session>> {
        let span = session_operation_span("get");
        let result = self.get_session(req).instrument(span.clone()).await;
        record_session_result(&span, &result);
        result.map_err(|error| match error {
            PostgresSessionError::InvalidScope => AdkError::new(
                ErrorComponent::Session,
                ErrorCategory::NotFound,
                "session.not_found",
                "the durable session was not found",
            ),
            other => other.into_adk(),
        })
    }

    async fn list(&self, req: ListRequest) -> adk_rust::Result<Vec<Box<dyn Session>>> {
        req.try_app_name()?;
        req.try_user_id()?;
        self.require_app_user(&req.app_name, &req.user_id)
            .map_err(PostgresSessionError::into_adk)?;
        let limit = req.limit.unwrap_or(1);
        if limit > self.limits.max_events {
            return Err(PostgresSessionError::ResourceExhausted.into_adk());
        }
        if limit == 0 || req.offset.unwrap_or(0) > 0 {
            return Ok(Vec::new());
        }
        match self
            .get_session(GetRequest {
                app_name: req.app_name,
                user_id: req.user_id,
                session_id: self.authority.session_id.clone(),
                num_recent_events: Some(0),
                after: None,
            })
            .await
        {
            Ok(session) => Ok(vec![session]),
            Err(PostgresSessionError::InvalidScope) => Ok(Vec::new()),
            Err(error) => Err(error.into_adk()),
        }
    }

    async fn delete(&self, req: DeleteRequest) -> adk_rust::Result<()> {
        req.try_identity()?;
        self.require_identity(&req.app_name, &req.user_id, &req.session_id)
            .map_err(PostgresSessionError::into_adk)?;
        let mut transaction = self
            .pool
            .begin()
            .await
            .map_err(storage_error)
            .map_err(PostgresSessionError::into_adk)?;
        self.lock_current_writer(&mut transaction, true)
            .await
            .map_err(PostgresSessionError::into_adk)?;
        sqlx::query(
            r"
DELETE FROM elitea_runtime.agent_sessions
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND thread_id = $7
  AND app_name = $8
  AND user_id = $9
  AND session_id = $10
            ",
        )
        .bind(&self.authority.tenant_id)
        .bind(self.authority.resource_project_id)
        .bind(self.authority.projection_project_id)
        .bind(self.authority.capability_id)
        .bind(SESSION_FAMILY)
        .bind(self.authority.definition_digest.as_slice())
        .bind(&self.authority.thread_id)
        .bind(&self.authority.app_name)
        .bind(&self.authority.user_id)
        .bind(&self.authority.session_id)
        .execute(&mut *transaction)
        .await
        .map_err(storage_error)
        .map_err(PostgresSessionError::into_adk)?;
        self.commit_current(transaction)
            .await
            .map_err(PostgresSessionError::into_adk)?;
        Ok(())
    }

    async fn append_event(&self, session_id: &str, event: Event) -> adk_rust::Result<()> {
        self.require_identity(
            &self.authority.app_name,
            &self.authority.user_id,
            session_id,
        )
        .map_err(PostgresSessionError::into_adk)?;
        let identity = adk_rust::AdkIdentity::new(
            adk_rust::AppName::try_from(self.authority.app_name.as_str())?,
            adk_rust::UserId::try_from(self.authority.user_id.as_str())?,
            adk_rust::SessionId::try_from(self.authority.session_id.as_str())?,
        );
        let span = session_operation_span("append_event");
        let result = self.append(&identity, event).instrument(span.clone()).await;
        record_session_result(&span, &result);
        result.map_err(PostgresSessionError::into_adk)
    }

    async fn append_event_for_identity(&self, req: AppendEventRequest) -> adk_rust::Result<()> {
        let span = session_operation_span("append_event");
        let result = self
            .append(&req.identity, req.event)
            .instrument(span.clone())
            .await;
        record_session_result(&span, &result);
        result.map_err(PostgresSessionError::into_adk)
    }

    async fn health_check(&self) -> adk_rust::Result<()> {
        let mut transaction = self
            .pool
            .begin()
            .await
            .map_err(storage_error)
            .map_err(PostgresSessionError::into_adk)?;
        self.lock_current_writer(&mut transaction, false)
            .await
            .map_err(PostgresSessionError::into_adk)?;
        self.commit_current(transaction)
            .await
            .map_err(PostgresSessionError::into_adk)
    }
}

fn session_operation_span(operation: &'static str) -> tracing::Span {
    tracing::info_span!(
        "agent.session.persist",
        backend = "postgres",
        operation,
        outcome = tracing::field::Empty,
        error_code = tracing::field::Empty,
        retryable = tracing::field::Empty,
    )
}

fn record_session_result<T>(span: &tracing::Span, result: &Result<T, PostgresSessionError>) {
    match result {
        Ok(_) => {
            span.record("outcome", "succeeded");
        }
        Err(error) => {
            span.record("outcome", "failed");
            span.record("error_code", error.code());
            span.record(
                "retryable",
                matches!(error, PostgresSessionError::StorageUnavailable { .. }),
            );
        }
    }
}

async fn activate_writer_row(
    transaction: &mut Transaction<'_, Postgres>,
    authority: &SessionWriterAuthority,
    claimed_at: DateTime<Utc>,
) -> Result<Option<String>, PostgresSessionError> {
    sqlx::query_scalar::<_, String>(
        r"
INSERT INTO elitea_runtime.agent_session_writers AS writer (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, thread_id, app_name, user_id,
    session_id, writer_claim_id, writer_execution_id, writer_generation,
    writer_claim_attempt, writer_lease_epoch, writer_claimed_at
)
VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
)
ON CONFLICT (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, thread_id, app_name, user_id, session_id
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
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.thread_id)
    .bind(&authority.app_name)
    .bind(&authority.user_id)
    .bind(&authority.session_id)
    .bind(&authority.claim_id)
    .bind(&authority.execution_id)
    .bind(authority.generation)
    .bind(authority.claim_attempt)
    .bind(authority.lease_epoch)
    .bind(claimed_at)
    .fetch_optional(&mut **transaction)
    .await
    .map_err(storage_error)
}

async fn insert_event(
    transaction: &mut Transaction<'_, Postgres>,
    authority: &SessionWriterAuthority,
    event: &Event,
    encoded_event: &str,
    ordinal: i64,
) -> Result<(), PostgresSessionError> {
    sqlx::query(
        r"
INSERT INTO elitea_runtime.agent_session_events (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, thread_id, app_name, user_id,
    session_id, event_id, event_ordinal, event_timestamp, event_payload,
    writer_claim_id, writer_execution_id, writer_generation,
    writer_claim_attempt, writer_lease_epoch
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19
)
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.thread_id)
    .bind(&authority.app_name)
    .bind(&authority.user_id)
    .bind(&authority.session_id)
    .bind(&event.id)
    .bind(ordinal)
    .bind(event.timestamp)
    .bind(encoded_event)
    .bind(&authority.claim_id)
    .bind(&authority.execution_id)
    .bind(authority.generation)
    .bind(authority.claim_attempt)
    .bind(authority.lease_epoch)
    .execute(&mut **transaction)
    .await
    .map_err(storage_error)?;
    Ok(())
}

async fn upsert_app_state(
    transaction: &mut Transaction<'_, Postgres>,
    authority: &SessionWriterAuthority,
    delta: &HashMap<String, Value>,
    updated_at: DateTime<Utc>,
    limits: SessionLimits,
) -> Result<(), PostgresSessionError> {
    let limits = SessionLimits {
        max_state_bytes: limits.max_state_bytes.min(MAX_SHARED_STATE_BYTES),
        ..limits
    };
    if delta.is_empty() {
        return Ok(());
    }
    let current = sqlx::query_scalar::<_, String>(
        r"
SELECT state
FROM elitea_runtime.agent_session_app_states
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND app_name = $7
FOR UPDATE
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.app_name)
    .fetch_optional(&mut **transaction)
    .await
    .map_err(storage_error)?;
    let mut state = current
        .as_deref()
        .map(|value| decode_state(value, limits))
        .transpose()?
        .unwrap_or_default();
    state.extend(delta.clone());
    let state = encode_state(&state, limits)?;
    sqlx::query(
        r"
INSERT INTO elitea_runtime.agent_session_app_states (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, app_name, state, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, app_name
)
DO UPDATE SET state = EXCLUDED.state, updated_at = EXCLUDED.updated_at
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.app_name)
    .bind(&state)
    .bind(updated_at)
    .execute(&mut **transaction)
    .await
    .map_err(storage_error)?;
    Ok(())
}

async fn upsert_user_state(
    transaction: &mut Transaction<'_, Postgres>,
    authority: &SessionWriterAuthority,
    delta: &HashMap<String, Value>,
    updated_at: DateTime<Utc>,
    limits: SessionLimits,
) -> Result<(), PostgresSessionError> {
    let limits = SessionLimits {
        max_state_bytes: limits.max_state_bytes.min(MAX_SHARED_STATE_BYTES),
        ..limits
    };
    if delta.is_empty() {
        return Ok(());
    }
    let current = sqlx::query_scalar::<_, String>(
        r"
SELECT state
FROM elitea_runtime.agent_session_user_states
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND app_name = $7
  AND user_id = $8
FOR UPDATE
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.app_name)
    .bind(&authority.user_id)
    .fetch_optional(&mut **transaction)
    .await
    .map_err(storage_error)?;
    let mut state = current
        .as_deref()
        .map(|value| decode_state(value, limits))
        .transpose()?
        .unwrap_or_default();
    state.extend(delta.clone());
    let state = encode_state(&state, limits)?;
    sqlx::query(
        r"
INSERT INTO elitea_runtime.agent_session_user_states (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, app_name, user_id, state, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, app_name, user_id
)
DO UPDATE SET state = EXCLUDED.state, updated_at = EXCLUDED.updated_at
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.app_name)
    .bind(&authority.user_id)
    .bind(&state)
    .bind(updated_at)
    .execute(&mut **transaction)
    .await
    .map_err(storage_error)?;
    Ok(())
}

async fn load_app_state(
    transaction: &mut Transaction<'_, Postgres>,
    authority: &SessionWriterAuthority,
    limits: SessionLimits,
) -> Result<HashMap<String, Value>, PostgresSessionError> {
    let limits = SessionLimits {
        max_state_bytes: limits.max_state_bytes.min(MAX_SHARED_STATE_BYTES),
        ..limits
    };
    let state = sqlx::query_scalar::<_, String>(
        r"
SELECT state
FROM elitea_runtime.agent_session_app_states
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND app_name = $7
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.app_name)
    .fetch_optional(&mut **transaction)
    .await
    .map_err(storage_error)?;
    state
        .as_deref()
        .map(|state| decode_state(state, limits))
        .transpose()
        .map(Option::unwrap_or_default)
}

async fn load_user_state(
    transaction: &mut Transaction<'_, Postgres>,
    authority: &SessionWriterAuthority,
    limits: SessionLimits,
) -> Result<HashMap<String, Value>, PostgresSessionError> {
    let limits = SessionLimits {
        max_state_bytes: limits.max_state_bytes.min(MAX_SHARED_STATE_BYTES),
        ..limits
    };
    let state = sqlx::query_scalar::<_, String>(
        r"
SELECT state
FROM elitea_runtime.agent_session_user_states
WHERE tenant_id = $1
  AND resource_project_id = $2
  AND projection_project_id = $3
  AND capability_id = $4
  AND session_family = $5
  AND definition_digest = $6
  AND app_name = $7
  AND user_id = $8
        ",
    )
    .bind(&authority.tenant_id)
    .bind(authority.resource_project_id)
    .bind(authority.projection_project_id)
    .bind(authority.capability_id)
    .bind(SESSION_FAMILY)
    .bind(authority.definition_digest.as_slice())
    .bind(&authority.app_name)
    .bind(&authority.user_id)
    .fetch_optional(&mut **transaction)
    .await
    .map_err(storage_error)?;
    state
        .as_deref()
        .map(|state| decode_state(state, limits))
        .transpose()
        .map(Option::unwrap_or_default)
}

fn encode_state(
    state: &HashMap<String, Value>,
    limits: SessionLimits,
) -> Result<String, PostgresSessionError> {
    validate_state(state, limits)?;
    canonical_json(
        &Value::Object(
            state
                .iter()
                .map(|(key, value)| (key.clone(), value.clone()))
                .collect(),
        ),
        limits.max_state_bytes,
    )
}

fn decode_state(
    encoded: &str,
    limits: SessionLimits,
) -> Result<HashMap<String, Value>, PostgresSessionError> {
    if encoded.len() > limits.max_state_bytes {
        return Err(PostgresSessionError::CorruptStoredState);
    }
    let state = decode_json::<HashMap<String, Value>>(encoded)?;
    validate_state(&state, limits).map_err(|_| PostgresSessionError::CorruptStoredState)?;
    Ok(state)
}

pub(super) fn encode_event(
    event: &Event,
    limits: SessionLimits,
) -> Result<String, PostgresSessionError> {
    let value =
        serde_json::to_value(event).map_err(|_| PostgresSessionError::CorruptStoredState)?;
    validate_json(&value, limits)?;
    canonical_json(&value, limits.max_event_bytes)
}

pub(super) fn decode_event(
    encoded: &str,
    limits: SessionLimits,
) -> Result<Event, PostgresSessionError> {
    if encoded.len() > limits.max_event_bytes {
        return Err(PostgresSessionError::CorruptStoredState);
    }
    let value = decode_json::<Value>(encoded)?;
    validate_json(&value, limits).map_err(|_| PostgresSessionError::CorruptStoredState)?;
    serde_json::from_value(value).map_err(|_| PostgresSessionError::CorruptStoredState)
}

fn validate_state(
    state: &HashMap<String, Value>,
    limits: SessionLimits,
) -> Result<(), PostgresSessionError> {
    if state.len() > limits.max_json_nodes {
        return Err(PostgresSessionError::ResourceExhausted);
    }
    for key in state.keys() {
        adk_rust::validate_state_key(key).map_err(|_| PostgresSessionError::InvalidScope)?;
    }
    let value = Value::Object(
        state
            .iter()
            .map(|(key, value)| (key.clone(), value.clone()))
            .collect(),
    );
    validate_json(&value, limits)?;
    Ok(())
}

fn validate_json(value: &Value, limits: SessionLimits) -> Result<(), PostgresSessionError> {
    let mut stack = vec![(value, 1_usize)];
    let mut nodes = 0_usize;
    while let Some((value, depth)) = stack.pop() {
        nodes = nodes
            .checked_add(1)
            .ok_or(PostgresSessionError::ResourceExhausted)?;
        if nodes > limits.max_json_nodes || depth > limits.max_json_depth {
            return Err(PostgresSessionError::ResourceExhausted);
        }
        match value {
            Value::Array(values) => {
                stack.extend(values.iter().map(|value| (value, depth + 1)));
            }
            Value::Object(values) => {
                for (key, value) in values {
                    if key.len() > MAX_IDENTITY_BYTES {
                        return Err(PostgresSessionError::ResourceExhausted);
                    }
                    stack.push((value, depth + 1));
                }
            }
            Value::String(value) if value.len() > MAX_EVENT_BYTES => {
                return Err(PostgresSessionError::ResourceExhausted);
            }
            Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
        }
    }
    Ok(())
}

pub(super) fn canonical_json(
    value: &Value,
    max_bytes: usize,
) -> Result<String, PostgresSessionError> {
    let canonical = canonicalize(value);
    let encoded =
        serde_json::to_string(&canonical).map_err(|_| PostgresSessionError::CorruptStoredState)?;
    if encoded.len() > max_bytes {
        return Err(PostgresSessionError::ResourceExhausted);
    }
    Ok(encoded)
}

fn canonicalize(value: &Value) -> Value {
    match value {
        Value::Array(values) => Value::Array(values.iter().map(canonicalize).collect()),
        Value::Object(values) => {
            let sorted = values
                .iter()
                .map(|(key, value)| (key.clone(), canonicalize(value)))
                .collect::<BTreeMap<_, _>>();
            Value::Object(sorted.into_iter().collect())
        }
        other => other.clone(),
    }
}

fn decode_json<T: DeserializeOwned>(encoded: &str) -> Result<T, PostgresSessionError> {
    serde_json::from_str(encoded).map_err(|_| PostgresSessionError::CorruptStoredState)
}

pub(super) fn postgres_timestamp(
    timestamp: DateTime<Utc>,
) -> Result<DateTime<Utc>, PostgresSessionError> {
    DateTime::from_timestamp_micros(timestamp.timestamp_micros())
        .ok_or(PostgresSessionError::CorruptStoredState)
}

fn bounded(value: &str, max_bytes: usize) -> bool {
    !value.is_empty() && value.len() <= max_bytes && !value.contains('\0')
}

fn storage_error(source: sqlx::Error) -> PostgresSessionError {
    match &source {
        sqlx::Error::Io(_)
        | sqlx::Error::Tls(_)
        | sqlx::Error::PoolTimedOut
        | sqlx::Error::PoolClosed
        | sqlx::Error::WorkerCrashed => PostgresSessionError::StorageUnavailable { source },
        sqlx::Error::Database(database)
            if matches!(
                database.code().as_deref(),
                Some("40001" | "40P01" | "55P03" | "57P01" | "57P02" | "57P03")
            ) =>
        {
            PostgresSessionError::StorageUnavailable { source }
        }
        _ => PostgresSessionError::StorageFailure { source },
    }
}

const CURRENT_WRITER_SHARED_SQL: &str = r"
SELECT 1
FROM elitea_runtime.agent_session_writers AS writer
WHERE writer.tenant_id = $1
  AND writer.resource_project_id = $2
  AND writer.projection_project_id = $3
  AND writer.capability_id = $4
  AND writer.session_family = $5
  AND writer.definition_digest = $6
  AND writer.thread_id = $7
  AND writer.app_name = $8
  AND writer.user_id = $9
  AND writer.session_id = $10
  AND writer.writer_claim_id = $11
  AND writer.writer_execution_id = $12
  AND writer.writer_generation = $13
  AND writer.writer_claim_attempt = $14
  AND writer.writer_lease_epoch = $15
FOR SHARE OF writer
";

const CURRENT_WRITER_EXCLUSIVE_SQL: &str = r"
SELECT 1
FROM elitea_runtime.agent_session_writers AS writer
WHERE writer.tenant_id = $1
  AND writer.resource_project_id = $2
  AND writer.projection_project_id = $3
  AND writer.capability_id = $4
  AND writer.session_family = $5
  AND writer.definition_digest = $6
  AND writer.thread_id = $7
  AND writer.app_name = $8
  AND writer.user_id = $9
  AND writer.session_id = $10
  AND writer.writer_claim_id = $11
  AND writer.writer_execution_id = $12
  AND writer.writer_generation = $13
  AND writer.writer_claim_attempt = $14
  AND writer.writer_lease_epoch = $15
FOR UPDATE OF writer
";
