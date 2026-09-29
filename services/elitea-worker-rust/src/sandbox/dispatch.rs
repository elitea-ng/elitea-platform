//! Worker-owned identities for durable stop delivery. Never stores code or grants.
use sqlx::{PgPool, Row};

pub(crate) struct DispatchScope {
    tenant: String,
    project: i32,
    execution: String,
    generation: i64,
}

impl DispatchScope {
    pub(crate) fn from_identity(
        identity: &crate::protocol::elitea::runtime::v1::ExecutionIdentityV1,
    ) -> Result<Self, DispatchError> {
        let project = identity
            .resource_project_id
            .parse::<i32>()
            .map_err(|_| DispatchError::Invalid)?;
        let generation = i64::try_from(identity.generation).map_err(|_| DispatchError::Invalid)?;
        if project <= 0
            || generation <= 0
            || identity.tenant_id.is_empty()
            || identity.tenant_id.len() > 256
            || identity.tenant_id.contains('\0')
            || identity.execution_id.is_empty()
            || identity.execution_id.len() > 256
            || identity.execution_id.contains('\0')
        {
            return Err(DispatchError::Invalid);
        }
        Ok(Self {
            tenant: identity.tenant_id.clone(),
            project,
            execution: identity.execution_id.clone(),
            generation,
        })
    }
}

#[derive(Debug, thiserror::Error)]
pub(crate) enum DispatchError {
    #[error("sandbox dispatch identity is invalid")]
    Invalid,
    #[error("sandbox activation conflicts with its persisted request or supervisor")]
    Conflict,
    #[error("sandbox dispatch identity could not be persisted")]
    Database(#[from] sqlx::Error),
}

pub(crate) struct DispatchJournal {
    pool: PgPool,
}

impl DispatchJournal {
    pub(crate) fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Commit before requesting a grant. Unknown submission outcomes keep this
    /// identity pending so cancellation can reach the original supervisor.
    pub(crate) async fn register(
        &self,
        scope: &DispatchScope,
        activation: &[u8; 32],
        digest: &[u8; 32],
        audience: &str,
    ) -> Result<(), DispatchError> {
        if audience.is_empty()
            || audience.len() > 256
            || audience
                .chars()
                .any(|c| c.is_control() || c.is_whitespace())
        {
            return Err(DispatchError::Invalid);
        }
        sqlx::query("INSERT INTO elitea_runtime.sandbox_dispatches (tenant_id,project_id,execution_id,generation,activation_id,request_digest,audience) VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING")
            .bind(&scope.tenant).bind(scope.project).bind(&scope.execution).bind(scope.generation)
            .bind(activation.as_slice()).bind(digest.as_slice()).bind(audience)
            .execute(&self.pool).await?;
        let row = sqlx::query("SELECT request_digest,audience FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND generation=$4 AND activation_id=$5")
            .bind(&scope.tenant).bind(scope.project).bind(&scope.execution).bind(scope.generation)
            .bind(activation.as_slice()).fetch_one(&self.pool).await?;
        let stored: Vec<u8> = row.try_get("request_digest")?;
        let target: String = row.try_get("audience")?;
        if stored != digest.as_slice() || target != audience {
            return Err(DispatchError::Conflict);
        }
        Ok(())
    }

    /// Only a confirmed terminal receipt resolves delivery; timeout does not.
    pub(crate) async fn resolve(
        &self,
        scope: &DispatchScope,
        activation: &[u8; 32],
        digest: &[u8; 32],
        audience: &str,
    ) -> Result<(), DispatchError> {
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_dispatches SET resolved=TRUE WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND generation=$4 AND activation_id=$5 AND request_digest=$6 AND audience=$7")
            .bind(&scope.tenant).bind(scope.project).bind(&scope.execution).bind(scope.generation)
            .bind(activation.as_slice()).bind(digest.as_slice()).bind(audience)
            .execute(&self.pool).await?.rows_affected();
        if count != 1 {
            return Err(DispatchError::Conflict);
        }
        Ok(())
    }
}
