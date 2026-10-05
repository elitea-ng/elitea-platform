//! Immutable repository runtime provenance in the original job receipt.
use super::{JobLease, JobLedger, JobScope, LedgerError, Phase, Row, changed, hex};
use serde::{Deserialize, Serialize};

#[derive(Clone, Eq, PartialEq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkspaceBackend {
    Docker,
    Kubernetes,
}

/// No Debug: repository metadata and opaque origin identities stay out of logs.
#[derive(Clone, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct WorkspaceRuntimeReceipt {
    pub revision: u8,
    pub backend: WorkspaceBackend,
    pub project_id: i32,
    pub job_key: String,
    pub request_digest: String,
    pub activation_id: String,
    pub original_runtime_id: String,
    pub volume_name: String,
    pub volume_owner_token: String,
    pub volume_creation_token: String,
    pub manifest_sha256: String,
}
fn digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
fn token(value: &str, limit: usize) -> bool {
    !value.is_empty() && value.len() <= limit && value.bytes().all(|b| (0x21..=0x7e).contains(&b))
}
impl WorkspaceRuntimeReceipt {
    /// # Errors
    /// Returns `LedgerError::Invalid` if the receipt and original runtime scope differ.
    pub fn validate_scope(&self, scope: &JobScope, runtime_id: &str) -> Result<(), LedgerError> {
        let identity = scope.runtime_identity()?;
        if self.revision != 1
            || self.project_id != scope.project
            || self.job_key != identity.job_key()
            || self.request_digest != hex(&scope.digest)
            || self.original_runtime_id != runtime_id
            || !digest(&self.activation_id)
            || !token(runtime_id, 512)
            || !digest(&self.manifest_sha256)
            || !token(&self.volume_owner_token, 128)
            || !token(&self.volume_creation_token, 128)
        {
            return Err(LedgerError::Invalid);
        }
        let expected = match self.backend {
            WorkspaceBackend::Docker => format!("elitea-code-repository-{}", self.job_key),
            WorkspaceBackend::Kubernetes => "repository".into(),
        };
        if self.volume_name != expected
            || self.backend == WorkspaceBackend::Docker
                && !(self.volume_owner_token.len() == 32
                    && self
                        .volume_owner_token
                        .bytes()
                        .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)))
            || self.backend == WorkspaceBackend::Kubernetes
                && (runtime_id.rsplit(':').next() != Some(self.volume_owner_token.as_str())
                    || self.volume_creation_token != self.volume_owner_token)
        {
            return Err(LedgerError::Invalid);
        }
        Ok(())
    }
    /// # Errors
    /// Returns `LedgerError::Invalid` if receipt serialization fails.
    pub fn to_transport(&self) -> Result<Vec<u8>, LedgerError> {
        serde_json::to_vec(self).map_err(|_| LedgerError::Invalid)
    }
    fn from_transport(bytes: &[u8]) -> Result<Self, LedgerError> {
        if bytes.len() > 4096 {
            return Err(LedgerError::Invalid);
        }
        let value: Self = serde_json::from_slice(bytes).map_err(|_| LedgerError::Invalid)?;
        if value.to_transport()? != bytes {
            return Err(LedgerError::Invalid);
        }
        Ok(value)
    }
}
impl JobLedger {
    /// Bind once while the original runtime still exists and before repository transfer.
    /// Replays compare exact bytes; stale writers never change the cleanup authority.
    /// # Errors
    /// Returns `Invalid` for invalid receipts and `Fenced` for a stale lease.
    /// Propagates schema and database errors.
    pub async fn bind_workspace_runtime(
        &self,
        lease: &JobLease,
        receipt: &WorkspaceRuntimeReceipt,
    ) -> Result<(), LedgerError> {
        receipt.validate_scope(&lease.scope, &receipt.original_runtime_id)?;
        let bytes = receipt.to_transport()?;
        let count=sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET workspace_runtime_receipt=$8,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND phase='reserved' AND runtime_id=$7 AND (workspace_runtime_receipt IS NULL OR workspace_runtime_receipt=$8)")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).bind(&receipt.original_runtime_id).bind(bytes).execute(&self.pool).await?.rows_affected();
        changed(count)
    }
    /// Terminal cleanup reads only the original row's immutable receipt.
    /// An absent proof cannot authorize deletion of a repository volume.
    /// # Errors
    /// Returns `Missing`, `Conflict`, or `Invalid` for absent, conflicting, or invalid job records.
    /// Propagates schema and database errors.
    pub async fn read_workspace_runtime(
        &self,
        scope: &JobScope,
    ) -> Result<Option<WorkspaceRuntimeReceipt>, LedgerError> {
        // Rolling deployments and legacy component schemas omit the optional column.
        // JSON lookup returns NULL there; it never manufactures a cleanup proof.
        let row=sqlx::query("SELECT request_digest,runtime_id,decode(substring(to_jsonb(sandbox_jobs)->>'workspace_runtime_receipt' from 3),'hex') AS workspace_runtime_receipt FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)?;
        if row.try_get::<Vec<u8>, _>("request_digest")? != scope.digest {
            return Err(LedgerError::Conflict);
        }
        let Some(bytes) = row.try_get::<Option<Vec<u8>>, _>("workspace_runtime_receipt")? else {
            return Ok(None);
        };
        let value = WorkspaceRuntimeReceipt::from_transport(&bytes)?;
        let runtime = row
            .try_get::<Option<String>, _>("runtime_id")?
            .ok_or(LedgerError::Invalid)?;
        value.validate_scope(scope, &runtime)?;
        Ok(Some(value))
    }
    /// # Errors
    /// Returns `LedgerError::Fenced` while the original job is active.
    /// Propagates the original job and receipt read errors.
    pub async fn terminal_workspace_runtime(
        &self,
        scope: &JobScope,
    ) -> Result<Option<WorkspaceRuntimeReceipt>, LedgerError> {
        if matches!(
            self.read(scope).await?.phase,
            Phase::Reserved | Phase::Dispatched
        ) {
            return Err(LedgerError::Fenced);
        }
        self.read_workspace_runtime(scope).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn provenance_cannot_cross_scope_or_accept_forged_runtime_and_volume() {
        let scope = JobScope::new("tenant".into(), 7, [1; 32], [2; 32]).unwrap();
        let job = scope.runtime_identity().unwrap();
        let base = WorkspaceRuntimeReceipt {
            revision: 1,
            backend: WorkspaceBackend::Docker,
            project_id: 7,
            job_key: job.job_key().into(),
            request_digest: job.request_digest().into(),
            activation_id: "a".repeat(64),
            original_runtime_id: "original-cid".into(),
            volume_name: format!("elitea-code-repository-{}", job.job_key()),
            volume_owner_token: "d".repeat(32),
            volume_creation_token: "2026-10-04T00:00:00Z".into(),
            manifest_sha256: "c".repeat(64),
        };
        base.validate_scope(&scope, "original-cid").unwrap();
        assert!(base.validate_scope(&scope, "replacement-cid").is_err());
        for field in 0..6 {
            let mut changed = base.clone();
            match field {
                0 => changed.project_id = 8,
                1 => changed.job_key = "e".repeat(64),
                2 => changed.request_digest = "f".repeat(64),
                3 => changed.volume_name = "foreign-volume".into(),
                4 => changed.volume_owner_token = String::new(),
                _ => changed.volume_owner_token = "not-a-volume-owner-token".into(),
            }
            assert!(changed.validate_scope(&scope, "original-cid").is_err());
        }
        let bytes = base.to_transport().unwrap();
        assert_eq!(
            WorkspaceRuntimeReceipt::from_transport(&bytes)
                .unwrap()
                .to_transport()
                .unwrap(),
            bytes
        );
    }
}
#[cfg(test)]
#[path = "ledger_workspace_postgres_tests.rs"]
mod postgres_tests;
