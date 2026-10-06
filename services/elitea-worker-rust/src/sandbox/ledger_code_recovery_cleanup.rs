//! Terminal cleanup authority is distinct from every executable job lease.
use super::{JobLedger, JobScope, LedgerError};
use crate::sandbox::code_recovery::{
    NO_EFFECT_FAILURE_CODE, WholeCodeBinding, WholeCodeRecoveryReceipt,
};
use sqlx::Row as _;

pub(crate) struct CodeNoEffectCleanupLease {
    scope: JobScope,
    owner: String,
    epoch: i64,
    runtime_id: String,
}
#[derive(Clone, Copy)]
pub(crate) enum CodeCleanupFailure {
    TerminationUnconfirmed,
    CleanupUnconfirmed,
}
impl CodeCleanupFailure {
    fn code(self) -> &'static str {
        match self {
            Self::TerminationUnconfirmed => "termination_unconfirmed",
            Self::CleanupUnconfirmed => "cleanup_unconfirmed",
        }
    }
}
impl CodeNoEffectCleanupLease {
    pub(crate) fn scope(&self) -> &JobScope {
        &self.scope
    }
    pub(crate) fn identity(
        &self,
    ) -> Result<adk_sandbox::workspace::docker::CodeJobIdentity, LedgerError> {
        self.scope
            .runtime_identity()?
            .with_runtime_id(self.runtime_id.clone())
            .map_err(|_| LedgerError::Invalid)
    }
}
impl JobLedger {
    /// This bounded discovery does not claim, reserve or expose executable work.
    pub(crate) async fn pending_code_no_effect_cleanup(
        &self,
        owner: &str,
        limit: i64,
    ) -> Result<Vec<JobScope>, LedgerError> {
        cleanup_bounds(owner, limit)?;
        let rows = sqlx::query("SELECT tenant_id,project_id,job_key,request_digest FROM elitea_runtime.sandbox_jobs WHERE owner_id=$1 AND phase='failed' AND failure_code=$2 AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_receipt_json IS NOT NULL AND runtime_id IS NOT NULL AND code_recovery_cleanup_at IS NULL AND (lease_until IS NULL OR lease_until <= clock_timestamp()) ORDER BY updated_at,tenant_id,project_id,job_key LIMIT $3")
            .bind(owner).bind(NO_EFFECT_FAILURE_CODE).bind(limit).fetch_all(&self.pool).await?;
        rows.into_iter()
            .map(|row| {
                let key: Vec<u8> = row.try_get("job_key")?;
                let digest: Vec<u8> = row.try_get("request_digest")?;
                JobScope::new(
                    row.try_get("tenant_id")?,
                    row.try_get("project_id")?,
                    key.try_into().map_err(|_| LedgerError::Invalid)?,
                    digest.try_into().map_err(|_| LedgerError::Invalid)?,
                )
            })
            .collect()
    }
    /// A terminal cleanup lease can never be passed to `mark_dispatched` or finish.
    pub(crate) async fn claim_code_no_effect_cleanup(
        &self,
        scope: &JobScope,
        owner: &str,
        seconds: i32,
    ) -> Result<Option<CodeNoEffectCleanupLease>, LedgerError> {
        cleanup_bounds(owner, 1)?;
        if !(1..=60).contains(&seconds) {
            return Err(LedgerError::Invalid);
        }
        let mut tx = self.pool.begin().await?;
        let row = sqlx::query("SELECT code_recovery_binding_json,code_recovery_receipt_json,runtime_id FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND phase='failed' AND failure_code=$6 AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_cleanup_at IS NULL AND runtime_id IS NOT NULL AND (lease_until IS NULL OR lease_until <= clock_timestamp()) FOR UPDATE")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(owner).bind(NO_EFFECT_FAILURE_CODE).fetch_optional(&mut *tx).await?;
        let Some(row) = row else {
            return Ok(None);
        };
        let binding: String = row.try_get("code_recovery_binding_json")?;
        let binding: WholeCodeBinding =
            serde_json::from_str(&binding).map_err(|_| LedgerError::Invalid)?;
        let receipt: String = row.try_get("code_recovery_receipt_json")?;
        let receipt = WholeCodeRecoveryReceipt::parse(receipt.as_bytes())
            .map_err(|_| LedgerError::Invalid)?;
        if !matches!(receipt, WholeCodeRecoveryReceipt::VerifiedNoEffect { .. })
            || receipt.binding() != &binding
            || binding.job_key != super::hex(&scope.key)
            || binding.request_digest != super::hex(&scope.digest)
        {
            return Err(LedgerError::Invalid);
        }
        let runtime_id: String = row.try_get("runtime_id")?;
        let epoch: Option<i64> = sqlx::query_scalar("UPDATE elitea_runtime.sandbox_jobs SET lease_epoch=lease_epoch+1,lease_until=clock_timestamp()+make_interval(secs => $7),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND phase='failed' AND failure_code=$6 AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_cleanup_at IS NULL AND lease_epoch < 9223372036854775807 AND (lease_until IS NULL OR lease_until <= clock_timestamp()) RETURNING lease_epoch")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(owner).bind(NO_EFFECT_FAILURE_CODE).bind(f64::from(seconds)).fetch_optional(&mut *tx).await?;
        let Some(epoch) = epoch else {
            return Ok(None);
        };
        let lease = CodeNoEffectCleanupLease {
            scope: scope.clone(),
            owner: owner.into(),
            epoch,
            runtime_id,
        };
        lease.identity()?;
        tx.commit().await?;
        Ok(Some(lease))
    }
    pub(crate) async fn check_code_no_effect_cleanup(
        &self,
        lease: &CodeNoEffectCleanupLease,
    ) -> Result<(), LedgerError> {
        let valid: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND runtime_id=$7 AND phase='failed' AND failure_code=$8 AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_cleanup_at IS NULL AND lease_until > clock_timestamp())")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).bind(&lease.runtime_id).bind(NO_EFFECT_FAILURE_CODE).fetch_one(&self.pool).await?;
        if valid {
            Ok(())
        } else {
            Err(LedgerError::Fenced)
        }
    }
    pub(crate) async fn record_code_no_effect_cleanup(
        &self,
        lease: &CodeNoEffectCleanupLease,
        failure: Option<CodeCleanupFailure>,
    ) -> Result<(), LedgerError> {
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET code_recovery_cleanup_at=CASE WHEN $9::text IS NULL THEN clock_timestamp() ELSE NULL END,code_recovery_cleanup_failure=$9,lease_until=clock_timestamp(),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND runtime_id=$7 AND phase='failed' AND failure_code=$8 AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_cleanup_at IS NULL AND lease_until > clock_timestamp()")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).bind(&lease.runtime_id).bind(NO_EFFECT_FAILURE_CODE).bind(failure.map(CodeCleanupFailure::code)).execute(&self.pool).await?.rows_affected();
        super::changed(count)
    }
}
fn cleanup_bounds(owner: &str, limit: i64) -> Result<(), LedgerError> {
    if owner.is_empty() || owner.len() > 128 || owner.contains('\0') || !(1..=32).contains(&limit) {
        Err(LedgerError::Invalid)
    } else {
        Ok(())
    }
}
