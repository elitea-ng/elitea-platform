//! Whole-Code owner storage. Missing rows and ambiguous dispatch never prove no effect.
use super::{JobLedger, JobScope, LedgerError, Phase};
use crate::{
    protocol::sandbox_grant::{AuthorizedCodeIntent, AuthorizedCodeRecovery, CodeOwnerOperation},
    sandbox::code_recovery::{
        NO_EFFECT_FAILURE_CODE, WholeCodeBinding, WholeCodeRecoveryReceipt, canonical,
    },
};
use serde::Serialize;
use sqlx::Row as _;

#[derive(Clone, Copy, Serialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum CodeOwnerState {
    Completed,
    VerifiedNoEffect,
    Pending,
    Failed,
    Cancelled,
    Uncertain,
    Missing,
    Conflict,
}
#[derive(Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct CodeOwnerObservation {
    pub(crate) schema: &'static str,
    pub(crate) state: CodeOwnerState,
    pub(crate) receipt: Option<WholeCodeRecoveryReceipt>,
}
impl CodeOwnerObservation {
    fn state(state: CodeOwnerState) -> Self {
        Self {
            schema: "elitea.sandbox.node-code-recovery-response.v1",
            state,
            receipt: None,
        }
    }
    fn receipt(state: CodeOwnerState, receipt: WholeCodeRecoveryReceipt) -> Self {
        Self {
            schema: "elitea.sandbox.node-code-recovery-response.v1",
            state,
            receipt: Some(receipt),
        }
    }
    pub(crate) fn canonical_bytes(&self) -> Result<Vec<u8>, LedgerError> {
        canonical(self).map_err(|_| LedgerError::Invalid)
    }
}
impl JobLedger {
    /// Register only from an independently signed original Execute intent, before claim/dispatch.
    /// This never upgrades a historical unbound job after dispatch or terminal settlement.
    pub(crate) async fn register_code_intent(
        &self,
        scope: &JobScope,
        intent: &AuthorizedCodeIntent,
    ) -> Result<(), LedgerError> {
        let binding = intent
            .binding(scope, chrono::Utc::now().timestamp_millis())
            .map_err(|_| LedgerError::Invalid)?;
        let bytes = binding
            .canonical_bytes()
            .map_err(|_| LedgerError::Invalid)?;
        let record = std::str::from_utf8(&bytes).map_err(|_| LedgerError::Invalid)?;
        let mut tx = self.pool.begin().await?;
        let row = sqlx::query("SELECT request_digest,phase,dispatched_at,code_recovery_binding_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 FOR UPDATE")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).fetch_optional(&mut *tx).await?.ok_or(LedgerError::Missing)?;
        let digest: Vec<u8> = row.try_get("request_digest")?;
        intent
            .binding(scope, chrono::Utc::now().timestamp_millis())
            .map_err(|_| LedgerError::Fenced)?;
        if digest.as_slice() != scope.digest {
            return Err(LedgerError::Conflict);
        }
        let prior: Option<String> = row.try_get("code_recovery_binding_json")?;
        match prior {
            Some(prior) if prior == record => {}
            Some(_) => return Err(LedgerError::Conflict),
            None => {
                // Recheck signature expiry after the lock wait. Never attest an already started job.
                intent
                    .binding(scope, chrono::Utc::now().timestamp_millis())
                    .map_err(|_| LedgerError::Fenced)?;
                let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET code_recovery_binding_json=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase='reserved' AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_binding_json IS NULL")
                    .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(record).execute(&mut *tx).await?.rows_affected();
                super::changed(count)?;
            }
        }
        tx.commit().await?;
        Ok(())
    }
    /// Read or seal on the original owner. No reserve/claim/runtime/Submit operation is called.
    pub(crate) async fn observe_code_owner(
        &self,
        authority: &AuthorizedCodeRecovery,
    ) -> Result<CodeOwnerObservation, LedgerError> {
        let scope = authority.scope();
        let mut tx = self.pool.begin().await?;
        let row = sqlx::query("SELECT request_digest,phase,result_json,failure_code,cancellation_requested,dispatched_at,code_recovery_binding_json,code_recovery_receipt_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 FOR UPDATE")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).fetch_optional(&mut *tx).await?;
        let Some(row) = row else {
            return Ok(CodeOwnerObservation::state(CodeOwnerState::Missing));
        };
        let digest: Vec<u8> = row.try_get("request_digest")?;
        if digest.as_slice() != scope.digest {
            return Ok(CodeOwnerObservation::state(CodeOwnerState::Conflict));
        }
        let Some(binding): Option<String> = row.try_get("code_recovery_binding_json")? else {
            return Ok(CodeOwnerObservation::state(CodeOwnerState::Conflict));
        };
        let binding: WholeCodeBinding =
            serde_json::from_str(&binding).map_err(|_| LedgerError::Invalid)?;
        if !authority.permits(&binding, chrono::Utc::now().timestamp_millis()) {
            return Err(LedgerError::Fenced);
        }
        let phase = Phase::parse(row.try_get("phase")?)?;
        let cancelled: bool = row.try_get("cancellation_requested")?;
        if cancelled {
            return Ok(CodeOwnerObservation::state(CodeOwnerState::Cancelled));
        }
        let stored: Option<String> = row.try_get("code_recovery_receipt_json")?;
        if let Some(stored) = stored {
            let receipt = WholeCodeRecoveryReceipt::parse(stored.as_bytes())
                .map_err(|_| LedgerError::Invalid)?;
            if receipt.binding() != &binding || receipt.visit() != authority.visit() {
                return Ok(CodeOwnerObservation::state(CodeOwnerState::Conflict));
            }
            let state = match (&receipt, phase) {
                (WholeCodeRecoveryReceipt::CommittedResult { .. }, Phase::Completed) => {
                    CodeOwnerState::Completed
                }
                (WholeCodeRecoveryReceipt::VerifiedNoEffect { .. }, Phase::Failed)
                    if row.try_get::<Option<String>, _>("failure_code")?.as_deref()
                        == Some(NO_EFFECT_FAILURE_CODE) =>
                {
                    CodeOwnerState::VerifiedNoEffect
                }
                _ => return Err(LedgerError::Invalid),
            };
            tx.commit().await?;
            return Ok(CodeOwnerObservation::receipt(state, receipt));
        }
        let (state, receipt) = match phase {
            Phase::Completed => {
                let result: String = row
                    .try_get::<Option<String>, _>("result_json")?
                    .ok_or(LedgerError::Invalid)?;
                let receipt = WholeCodeRecoveryReceipt::committed(
                    binding,
                    authority.visit().clone(),
                    result.as_bytes(),
                )
                .map_err(|_| LedgerError::Invalid)?;
                (CodeOwnerState::Completed, receipt)
            }
            Phase::Reserved if authority.operation() == CodeOwnerOperation::SealNoEffect => {
                let receipt =
                    WholeCodeRecoveryReceipt::sealed_no_effect(binding, authority.visit().clone())
                        .map_err(|_| LedgerError::Invalid)?;
                let bytes = receipt
                    .canonical_bytes()
                    .map_err(|_| LedgerError::Invalid)?;
                let record = std::str::from_utf8(&bytes).map_err(|_| LedgerError::Invalid)?;
                // The same row lock and phase CAS serialize against every plain/compiled mark_dispatched.
                // Expire/increment the owner epoch; a retained inert allocation cannot signal user code.
                let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET phase='failed',failure_code=$5,code_recovery_receipt_json=$6,owner_id=COALESCE(owner_id,'node-code-recovery-seal'),lease_epoch=lease_epoch+1,lease_until=clock_timestamp(),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase='reserved' AND dispatched_at IS NULL AND NOT cancellation_requested AND lease_epoch < 9223372036854775807 AND code_recovery_receipt_json IS NULL")
                    .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(NO_EFFECT_FAILURE_CODE).bind(record).execute(&mut *tx).await?.rows_affected();
                super::changed(count)?;
                tx.commit().await?;
                return Ok(CodeOwnerObservation::receipt(
                    CodeOwnerState::VerifiedNoEffect,
                    receipt,
                ));
            }
            Phase::Reserved | Phase::Dispatched => {
                return Ok(CodeOwnerObservation::state(CodeOwnerState::Pending));
            }
            Phase::Failed => return Ok(CodeOwnerObservation::state(CodeOwnerState::Failed)),
            Phase::Cancelled => return Ok(CodeOwnerObservation::state(CodeOwnerState::Cancelled)),
            Phase::Uncertain => return Ok(CodeOwnerObservation::state(CodeOwnerState::Uncertain)),
        };
        let bytes = receipt
            .canonical_bytes()
            .map_err(|_| LedgerError::Invalid)?;
        let record = std::str::from_utf8(&bytes).map_err(|_| LedgerError::Invalid)?;
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET code_recovery_receipt_json=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase='completed' AND NOT cancellation_requested AND code_recovery_receipt_json IS NULL")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(record).execute(&mut *tx).await?.rows_affected();
        super::changed(count)?;
        tx.commit().await?;
        Ok(CodeOwnerObservation::receipt(state, receipt))
    }
}

#[cfg(test)]
#[path = "ledger_code_recovery_tests.rs"]
mod tests;
