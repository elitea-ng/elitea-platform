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
    #[error("sandbox stop could not be delivered: {0}")]
    Stop(#[from] super::client::SandboxCallError),
    #[error("the original sandbox supervisor is not configured; stop remains pending")]
    TargetUnavailable,
    #[error("sandbox dispatch identity could not be persisted")]
    Database(#[from] sqlx::Error),
}

#[async_trait::async_trait]
pub(crate) trait SandboxStopDelivery: Send + Sync {
    /// True only when every registered runtime has a confirmed terminal receipt.
    async fn stop(
        &self,
        authority: &crate::protocol::control::SandboxStopAuthority,
    ) -> Result<bool, DispatchError>;
}

pub(crate) struct PendingDispatch {
    pub(crate) activation: [u8; 32],
    pub(crate) digest: [u8; 32],
    pub(crate) audience: String,
    pub(crate) descriptor: Option<Vec<u8>>,
}

pub(crate) struct DispatchJournal {
    pool: PgPool,
}

impl DispatchJournal {
    /// Preserve prior preparation admission across claim generations and terminal delivery.
    pub(crate) async fn contains_activation(
        &self,
        scope: &DispatchScope,
        activation: &[u8; 32],
    ) -> Result<bool, DispatchError> {
        Ok(sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND activation_id=$4)",
        )
        .bind(&scope.tenant).bind(scope.project).bind(&scope.execution)
        .bind(activation.as_slice()).fetch_one(&self.pool).await?)
    }

    /// Same answer as `contains_activation` for each id, in one `= ANY($4)` round trip.
    pub(crate) async fn contains_activations(
        &self,
        scope: &DispatchScope,
        activations: &[[u8; 32]],
    ) -> Result<Vec<bool>, DispatchError> {
        if activations.is_empty() || activations.len() > 8 {
            return Err(DispatchError::Invalid);
        }
        let ids: Vec<&[u8]> = activations.iter().map(<[u8; 32]>::as_slice).collect();
        let found: Vec<Vec<u8>> = sqlx::query_scalar(
            "SELECT DISTINCT activation_id FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND activation_id = ANY($4::bytea[])",
        )
        .bind(&scope.tenant).bind(scope.project).bind(&scope.execution)
        .bind(&ids).fetch_all(&self.pool).await?;
        Ok(activations
            .iter()
            .map(|id| found.iter().any(|row| row.as_slice() == id.as_slice()))
            .collect())
    }

    /// Preserve the original request and audience across claim generations.
    pub(crate) async fn recorded(
        &self,
        scope: &DispatchScope,
        activation: &[u8; 32],
        snapshot_metadata: bool,
    ) -> Result<Option<PendingDispatch>, DispatchError> {
        let query = if snapshot_metadata {
            "SELECT request_digest,audience,compiled_descriptor_json FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND activation_id=$4 ORDER BY generation LIMIT 1025"
        } else {
            "SELECT request_digest,audience,NULL::bytea AS compiled_descriptor_json FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND activation_id=$4 ORDER BY generation LIMIT 1025"
        };
        let rows = sqlx::query(query)
            .bind(&scope.tenant)
            .bind(scope.project)
            .bind(&scope.execution)
            .bind(activation.as_slice())
            .fetch_all(&self.pool)
            .await?;
        if rows.len() > 1024 {
            return Err(DispatchError::Invalid);
        }
        let mut original: Option<PendingDispatch> = None;
        for row in rows {
            let digest: Vec<u8> = row.try_get("request_digest")?;
            let digest = digest.try_into().map_err(|_| DispatchError::Invalid)?;
            let audience: String = row.try_get("audience")?;
            let descriptor: Option<Vec<u8>> = row.try_get("compiled_descriptor_json")?;
            if let Some(original) = &mut original {
                if original.digest != digest
                    || original.audience != audience
                    || original
                        .descriptor
                        .as_ref()
                        .zip(descriptor.as_ref())
                        .is_some_and(|(left, right)| left != right)
                {
                    return Err(DispatchError::Conflict);
                }
                if original.descriptor.is_none() {
                    original.descriptor = descriptor;
                }
            } else {
                original = Some(PendingDispatch {
                    activation: *activation,
                    digest,
                    audience,
                    descriptor,
                });
            }
        }
        Ok(original)
    }
    /// Persist a selected canonical descriptor before admission, across claim generations.
    pub(crate) async fn record_descriptor(
        &self,
        scope: &DispatchScope,
        activation: &[u8; 32],
        digest: &[u8; 32],
        audience: &str,
        descriptor: &[u8],
    ) -> Result<(), DispatchError> {
        if descriptor.is_empty() || descriptor.len() > 16 * 1024 {
            return Err(DispatchError::Invalid);
        }
        let mut tx = self.pool.begin().await?;
        let rows=sqlx::query("SELECT request_digest,audience,compiled_descriptor_json FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND activation_id=$4 FOR UPDATE")
            .bind(&scope.tenant).bind(scope.project).bind(&scope.execution).bind(activation.as_slice()).fetch_all(&mut *tx).await?;
        if rows.is_empty() {
            return Err(DispatchError::Conflict);
        }
        for row in rows {
            let existing: Option<Vec<u8>> = row.try_get("compiled_descriptor_json")?;
            let request: Vec<u8> = row.try_get("request_digest")?;
            let target: String = row.try_get("audience")?;
            if request.as_slice() != digest
                || target != audience
                || existing.as_deref().is_some_and(|value| value != descriptor)
            {
                return Err(DispatchError::Conflict);
            }
        }
        sqlx::query("UPDATE elitea_runtime.sandbox_dispatches SET compiled_descriptor_json=$5 WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND activation_id=$4 AND compiled_descriptor_json IS NULL")
            .bind(&scope.tenant).bind(scope.project).bind(&scope.execution).bind(activation.as_slice()).bind(descriptor).execute(&mut *tx).await?;
        tx.commit().await?;
        Ok(())
    }

    pub(crate) async fn pending(
        &self,
        scope: &DispatchScope,
    ) -> Result<Vec<PendingDispatch>, DispatchError> {
        let rows = sqlx::query("SELECT activation_id,request_digest,audience FROM elitea_runtime.sandbox_dispatches WHERE tenant_id=$1 AND project_id=$2 AND execution_id=$3 AND generation=$4 AND NOT resolved ORDER BY activation_id LIMIT 32")
            .bind(&scope.tenant).bind(scope.project).bind(&scope.execution).bind(scope.generation).fetch_all(&self.pool).await?;
        rows.into_iter()
            .map(|row| {
                let activation: Vec<u8> = row.try_get("activation_id")?;
                let digest: Vec<u8> = row.try_get("request_digest")?;
                Ok(PendingDispatch {
                    activation: activation.try_into().map_err(|_| DispatchError::Invalid)?,
                    digest: digest.try_into().map_err(|_| DispatchError::Invalid)?,
                    audience: row.try_get("audience")?,
                    descriptor: None,
                })
            })
            .collect()
    }

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

pub(crate) struct BoundSandboxStop {
    delivery: std::sync::Arc<dyn SandboxStopDelivery>,
    authority: crate::protocol::control::SandboxStopAuthority,
}
impl BoundSandboxStop {
    pub(crate) fn new(
        delivery: std::sync::Arc<dyn SandboxStopDelivery>,
        authority: crate::protocol::control::SandboxStopAuthority,
    ) -> Self {
        Self {
            delivery,
            authority,
        }
    }
    pub(crate) async fn confirmed(&self) -> bool {
        match tokio::time::timeout(
            std::time::Duration::from_secs(10),
            self.delivery.stop(&self.authority),
        )
        .await
        {
            Ok(Ok(complete)) => complete,
            Ok(Err(error)) => {
                tracing::error!(error = %error, "sandbox stop delivery failed; execution cancellation remains pending");
                false
            }
            Err(_) => {
                tracing::warn!(
                    "sandbox stop delivery timed out; execution cancellation remains pending"
                );
                false
            }
        }
    }
}
