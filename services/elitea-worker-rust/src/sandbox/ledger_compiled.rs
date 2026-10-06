//! Private compiler provenance under the existing job lease and request fence.
use super::{JobLease, JobLedger, JobScope, LedgerError, Row, changed};
use crate::sandbox::compiled_snapshot::{Control, Descriptor, Purpose};
pub(crate) struct CompiledRecord {
    pub(crate) descriptor: Option<Vec<u8>>,
    pub(crate) export_verified: bool,
    pub(crate) cleanup_confirmed: bool,
    pub(crate) export_epoch: Option<i64>,
}
impl JobLedger {
    pub(crate) async fn record_compiled_intent(
        &self,
        lease: &JobLease,
        control: &Control,
        purpose: Purpose,
        descriptor: Option<&[u8]>,
    ) -> Result<(), LedgerError> {
        let key = control
            .snapshot_key_sha256
            .raw()
            .map_err(|_| LedgerError::Invalid)?;
        let base = control
            .binding
            .base_prepared_request_sha256
            .raw()
            .map_err(|_| LedgerError::Invalid)?;
        if control
            .intent_digest(purpose)
            .map_err(|_| LedgerError::Invalid)?
            != lease.scope.digest
        {
            return Err(LedgerError::Conflict);
        }
        let purpose = match purpose {
            Purpose::Compile => "compile",
            Purpose::Execute => "execute",
        };
        let mut tx = self.pool.begin().await?;
        let row=sqlx::query("WITH owned AS MATERIALIZED (SELECT compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json,lease_until FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND phase='reserved' AND NOT cancellation_requested FOR UPDATE) SELECT * FROM owned WHERE lease_until>clock_timestamp()")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).fetch_optional(&mut *tx).await?.ok_or(LedgerError::Fenced)?;
        let existing: Option<String> = row.try_get("compiled_purpose")?;
        if let Some(existing) = existing {
            let recorded_key: Option<Vec<u8>> = row.try_get("compiled_snapshot_key")?;
            let recorded_base: Option<Vec<u8>> = row.try_get("compiled_base_request_digest")?;
            let recorded_descriptor: Option<Vec<u8>> = row.try_get("compiled_descriptor_json")?;
            if recorded_descriptor.as_deref() != descriptor
                || existing != purpose
                || recorded_key.as_deref() != Some(key.as_slice())
                || recorded_base.as_deref() != Some(base.as_slice())
            {
                return Err(LedgerError::Conflict);
            }
        } else {
            let count=sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET compiled_purpose=$7,compiled_snapshot_key=$8,compiled_base_request_digest=$9,compiled_descriptor_json=$10,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND phase='reserved' AND lease_until>clock_timestamp() AND NOT cancellation_requested AND compiled_purpose IS NULL AND runtime_id IS NULL")
                .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).bind(purpose).bind(key.as_slice()).bind(base.as_slice()).bind(descriptor).execute(&mut *tx).await?.rows_affected();
            changed(count)?;
        }
        tx.commit().await?;
        Ok(())
    }
    pub(crate) async fn read_compiled(
        &self,
        scope: &JobScope,
        control: &Control,
        purpose: Purpose,
    ) -> Result<CompiledRecord, LedgerError> {
        let row=sqlx::query("SELECT request_digest,compiled_purpose,compiled_snapshot_key,compiled_base_request_digest,compiled_descriptor_json,compiled_export_verified,runtime_cleanup_confirmed_at IS NOT NULL AS cleaned,compiled_export_lease_epoch FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)?;
        let request: Vec<u8> = row.try_get("request_digest")?;
        let kind: Option<String> = row.try_get("compiled_purpose")?;
        let key: Option<Vec<u8>> = row.try_get("compiled_snapshot_key")?;
        let base: Option<Vec<u8>> = row.try_get("compiled_base_request_digest")?;
        if request != scope.digest
            || kind.as_deref()
                != Some(match purpose {
                    Purpose::Compile => "compile",
                    Purpose::Execute => "execute",
                })
            || key.as_deref()
                != Some(
                    control
                        .snapshot_key_sha256
                        .raw()
                        .map_err(|_| LedgerError::Invalid)?
                        .as_slice(),
                )
            || base.as_deref()
                != Some(
                    control
                        .binding
                        .base_prepared_request_sha256
                        .raw()
                        .map_err(|_| LedgerError::Invalid)?
                        .as_slice(),
                )
        {
            return Err(LedgerError::Conflict);
        }
        Ok(CompiledRecord {
            descriptor: row.try_get("compiled_descriptor_json")?,
            export_verified: row.try_get("compiled_export_verified")?,
            cleanup_confirmed: row.try_get("cleaned")?,
            export_epoch: row.try_get("compiled_export_lease_epoch")?,
        })
    }
    pub(crate) async fn compiled_descriptor(
        &self,
        scope: &JobScope,
    ) -> Result<Vec<u8>, LedgerError> {
        sqlx::query_scalar("SELECT compiled_descriptor_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND compiled_purpose='compile' AND compiled_export_verified")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).fetch_optional(&self.pool).await?.flatten().ok_or(LedgerError::Missing)
    }
    pub(crate) async fn record_compiled_export(
        &self,
        lease: &JobLease,
        descriptor: &Descriptor,
        runtime_id: &str,
    ) -> Result<(), LedgerError> {
        let bytes = descriptor.bytes().map_err(|_| LedgerError::Invalid)?;
        let mut tx = self.pool.begin().await?;
        let row=sqlx::query("WITH owned AS MATERIALIZED (SELECT compiled_descriptor_json,compiled_export_lease_epoch,compiled_snapshot_key,compiled_base_request_digest,lease_until FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND phase='dispatched' AND compiled_purpose='compile' AND runtime_id=$7 AND NOT cancellation_requested FOR UPDATE) SELECT * FROM owned WHERE lease_until>clock_timestamp()")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).bind(runtime_id).fetch_optional(&mut *tx).await?.ok_or(LedgerError::Fenced)?;
        let existing: Option<Vec<u8>> = row.try_get("compiled_descriptor_json")?;
        let key: Option<Vec<u8>> = row.try_get("compiled_snapshot_key")?;
        let base: Option<Vec<u8>> = row.try_get("compiled_base_request_digest")?;
        if existing.as_ref().is_some_and(|value| *value != bytes)
            || key.as_deref()
                != Some(
                    descriptor
                        .snapshot_key_sha256
                        .raw()
                        .map_err(|_| LedgerError::Invalid)?
                        .as_slice(),
                )
            || base.as_deref()
                != Some(
                    descriptor
                        .binding
                        .base_prepared_request_sha256
                        .raw()
                        .map_err(|_| LedgerError::Invalid)?
                        .as_slice(),
                )
        {
            return Err(LedgerError::Conflict);
        }
        let count=sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET compiled_descriptor_json=$8,compiled_export_verified=true,compiled_export_lease_epoch=COALESCE(compiled_export_lease_epoch,$6),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND phase='dispatched' AND runtime_id=$7 AND lease_until>clock_timestamp() AND NOT cancellation_requested")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).bind(runtime_id).bind(bytes).execute(&mut *tx).await?.rows_affected();
        changed(count)?;
        tx.commit().await?;
        Ok(())
    }
    pub(crate) async fn confirm_compiled_cleanup(
        &self,
        scope: &JobScope,
        runtime_id: &str,
    ) -> Result<(), LedgerError> {
        let count=sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET runtime_cleanup_confirmed_at=COALESCE(runtime_cleanup_confirmed_at,clock_timestamp()),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND runtime_id=$5 AND phase IN ('completed','failed','cancelled','uncertain') AND compiled_purpose IS NOT NULL")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(runtime_id).execute(&self.pool).await?.rows_affected();
        changed(count)
    }
}
