#[allow(
    clippy::wildcard_imports,
    reason = "Share the owner module imports with runtime code and its existing tests."
)]
use super::*;
use crate::{
    protocol::sandbox_grant::{AuthorizedCodeIntent, AuthorizedCodePlatformOwner},
    sandbox::{
        code_platform_owner::{
            CodePlatformBinding, CodePlatformLedgerObservation, OwnedCodePlatformRuntime,
        },
        code_recovery::{WholeCodeBinding, canonical, whole_result},
        request::PreparedJob,
    },
};
impl JobLedger {
    pub(crate) async fn register_code_platform_binding(
        &self,
        scope: &JobScope,
        intent: &AuthorizedCodeIntent,
        job: &PreparedJob,
    ) -> Result<(), LedgerError> {
        self.register_code_platform_binding_selected(scope, intent, job, None)
            .await
    }
    pub(crate) async fn register_compiled_code_platform_binding(
        &self,
        scope: &JobScope,
        intent: &AuthorizedCodeIntent,
        job: &PreparedJob,
        control: &crate::sandbox::compiled_snapshot::Control,
        descriptor: &[u8],
    ) -> Result<(), LedgerError> {
        self.register_code_platform_binding_selected(
            scope,
            intent,
            job,
            Some((control, descriptor)),
        )
        .await
    }
    async fn register_code_platform_binding_selected(
        &self,
        scope: &JobScope,
        intent: &AuthorizedCodeIntent,
        job: &PreparedJob,
        selected: Option<(&crate::sandbox::compiled_snapshot::Control, &[u8])>,
    ) -> Result<(), LedgerError> {
        let whole = intent
            .binding(scope, chrono::Utc::now().timestamp_millis())
            .map_err(|_| LedgerError::Fenced)?;
        if !job.matches_code_binding(whole) {
            return Err(LedgerError::Invalid);
        }
        let broker = match selected {
            Some((control, descriptor)) => {
                CodePlatformBinding::from_compiled_job(job, whole, control, descriptor)
            }
            None => CodePlatformBinding::from_job(job, whole),
        }
        .map_err(|_| LedgerError::Invalid)?;
        let Some(broker) = broker else {
            return Ok(());
        };
        let mut tx = self.pool.begin().await?;
        let row=sqlx::query("SELECT code_recovery_binding_json,code_platform_binding_json,phase FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 FOR UPDATE")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).fetch_optional(&mut *tx).await?.ok_or(LedgerError::Missing)?;
        intent
            .binding(scope, chrono::Utc::now().timestamp_millis())
            .map_err(|_| LedgerError::Fenced)?;
        let whole = String::from_utf8(whole.canonical_bytes().map_err(|_| LedgerError::Invalid)?)
            .map_err(|_| LedgerError::Invalid)?;
        if row
            .try_get::<Option<String>, _>("code_recovery_binding_json")?
            .as_deref()
            != Some(whole.as_str())
        {
            return Err(LedgerError::Conflict);
        }
        let broker = String::from_utf8(canonical(&broker).map_err(|_| LedgerError::Invalid)?)
            .map_err(|_| LedgerError::Invalid)?;
        match row.try_get::<Option<String>, _>("code_platform_binding_json")? {
            Some(prior) if prior == broker => {}
            Some(_) => return Err(LedgerError::Conflict),
            None => {
                let changed=sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET code_platform_binding_json=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase='reserved' AND dispatched_at IS NULL AND NOT cancellation_requested AND code_platform_binding_json IS NULL")
                    .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(broker).execute(&mut *tx).await?.rows_affected();
                super::changed(changed)?;
            }
        }
        tx.commit().await?;
        Ok(())
    }
    /// Observe admission or durable completion without runtime or reply access.
    pub(crate) async fn observe_code_platform_runtime(
        &self,
        authority: &AuthorizedCodePlatformOwner,
    ) -> Result<CodePlatformLedgerObservation, LedgerError> {
        let scope = authority.scope();
        let record = self.read_code_platform_observation(authority).await?;
        if self.code_platform_completed(authority, record.as_ref())? {
            return Ok(CodePlatformLedgerObservation::Completed);
        }
        let now = chrono::Utc::now().timestamp_millis();
        let permitted = authority.permits_not_ready(
            record.as_ref().and_then(|r| r.binding.as_ref()),
            record.as_ref().and_then(|r| r.broker.as_ref()),
            now,
        );
        if admission_not_ready(record.as_ref().map(|r| &r.state), &scope.digest, permitted)? {
            return Ok(CodePlatformLedgerObservation::NotReady);
        }
        // Keep every existing dispatched owner, lease, binding and runtime fence.
        self.read_code_platform_runtime(authority)
            .await
            .map(CodePlatformLedgerObservation::Running)
    }
    /// Recheck exact durable completion after a read operation races cleanup.
    /// This never grants mailbox access, publishes a reply, or writes a receipt.
    pub(crate) async fn observe_code_platform_completion(
        &self,
        authority: &AuthorizedCodePlatformOwner,
    ) -> Result<bool, LedgerError> {
        let record = self.read_code_platform_observation(authority).await?;
        self.code_platform_completed(authority, record.as_ref())
    }
    #[allow(
        clippy::unused_self,
        reason = "Retain the existing ledger owner interface."
    )]
    fn code_platform_completed(
        &self,
        authority: &AuthorizedCodePlatformOwner,
        record: Option<&PlatformObservationRecord>,
    ) -> Result<bool, LedgerError> {
        let permitted = record.is_some_and(|record| {
            record
                .binding
                .as_ref()
                .zip(record.broker.as_ref())
                .is_some_and(|(binding, broker)| {
                    authority.permits_completed(
                        binding,
                        broker,
                        chrono::Utc::now().timestamp_millis(),
                    )
                })
        });
        completed_observation(
            record.map(|r| &r.state),
            &authority.scope().digest,
            permitted,
            record.and_then(|r| r.result.as_deref()),
        )
    }
    async fn read_code_platform_observation(
        &self,
        authority: &AuthorizedCodePlatformOwner,
    ) -> Result<Option<PlatformObservationRecord>, LedgerError> {
        let scope = authority.scope();
        // Select identity before digest filtering. Conflicts cannot become absence.
        let row = sqlx::query("SELECT request_digest,phase,dispatched_at IS NOT NULL AS was_dispatched,cancellation_requested,code_recovery_receipt_json IS NOT NULL AS has_receipt,(runtime_id IS NOT NULL AND owner_id IS NOT NULL AND lease_epoch>0 AND COALESCE(lease_until > clock_timestamp(),FALSE)) AS running_owner,code_recovery_binding_json,code_platform_binding_json,result_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).fetch_optional(&self.pool).await?;
        let Some(row) = row else { return Ok(None) };
        let binding = row
            .try_get::<Option<String>, _>("code_recovery_binding_json")?
            .map(|raw| {
                serde_json::from_str::<WholeCodeBinding>(&raw).map_err(|_| LedgerError::Invalid)
            })
            .transpose()?;
        let broker = row
            .try_get::<Option<String>, _>("code_platform_binding_json")?
            .map(|raw| {
                serde_json::from_str::<CodePlatformBinding>(&raw).map_err(|_| LedgerError::Invalid)
            })
            .transpose()?;
        Ok(Some(PlatformObservationRecord {
            state: PlatformAdmissionState {
                digest: row.try_get("request_digest")?,
                phase: Phase::parse(row.try_get("phase")?)?,
                was_dispatched: row.try_get("was_dispatched")?,
                cancelled: row.try_get("cancellation_requested")?,
                has_receipt: row.try_get("has_receipt")?,
                running_owner: row.try_get("running_owner")?,
            },
            binding,
            broker,
            result: row.try_get("result_json")?,
        }))
    }
    pub(crate) async fn read_code_platform_runtime(
        &self,
        authority: &AuthorizedCodePlatformOwner,
    ) -> Result<OwnedCodePlatformRuntime, LedgerError> {
        let scope = authority.scope();
        let row=sqlx::query("SELECT runtime_id,owner_id,lease_epoch,code_recovery_binding_json,code_platform_binding_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase='dispatched' AND dispatched_at IS NOT NULL AND runtime_id IS NOT NULL AND owner_id IS NOT NULL AND lease_until > clock_timestamp() AND NOT cancellation_requested AND code_recovery_receipt_json IS NULL")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).fetch_optional(&self.pool).await?.ok_or(LedgerError::Fenced)?;
        let binding: WholeCodeBinding =
            serde_json::from_str(&row.try_get::<String, _>("code_recovery_binding_json")?)
                .map_err(|_| LedgerError::Invalid)?;
        let broker: CodePlatformBinding =
            serde_json::from_str(&row.try_get::<String, _>("code_platform_binding_json")?)
                .map_err(|_| LedgerError::Invalid)?;
        if !authority.permits(&binding, &broker, chrono::Utc::now().timestamp_millis()) {
            return Err(LedgerError::Fenced);
        }
        let result = OwnedCodePlatformRuntime {
            runtime_id: row.try_get("runtime_id")?,
            owner: row.try_get("owner_id")?,
            epoch: row.try_get("lease_epoch")?,
            binding,
            broker,
        };
        if result.epoch <= 0
            || result.runtime_id.is_empty()
            || result.runtime_id.len() > 512
            || result
                .runtime_id
                .chars()
                .any(|c| c.is_control() || c.is_whitespace())
        {
            return Err(LedgerError::Invalid);
        }
        Ok(result)
    }
    pub(crate) async fn verify_code_platform_runtime(
        &self,
        authority: &AuthorizedCodePlatformOwner,
        prior: &OwnedCodePlatformRuntime,
    ) -> Result<(), LedgerError> {
        let current = self.read_code_platform_runtime(authority).await?;
        verify_unchanged_platform_owner(&current, prior)
    }
    /// Original inert admission only, under the actual retained owner lease.
    /// A later owner-read route cannot create this immutable launch binding.
    pub(crate) async fn code_platform_launch_for_admission(
        &self,
        lease: &JobLease,
    ) -> Result<Option<Vec<u8>>, LedgerError> {
        let scope = &lease.scope;
        let row=sqlx::query("SELECT runtime_id,code_recovery_binding_json,code_platform_binding_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND phase='reserved' AND dispatched_at IS NULL AND NOT cancellation_requested")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(&lease.owner).bind(lease.epoch).fetch_optional(&self.pool).await?.ok_or(LedgerError::Fenced)?;
        let Some(broker) = row.try_get::<Option<String>, _>("code_platform_binding_json")? else {
            return Ok(None);
        };
        let binding: WholeCodeBinding =
            serde_json::from_str(&row.try_get::<String, _>("code_recovery_binding_json")?)
                .map_err(|_| LedgerError::Invalid)?;
        let broker: CodePlatformBinding =
            serde_json::from_str(&broker).map_err(|_| LedgerError::Invalid)?;
        if !broker.matches(&binding) {
            return Err(LedgerError::Invalid);
        }
        let id: String = row.try_get("runtime_id")?;
        if id.is_empty() {
            return Err(LedgerError::Invalid);
        }
        broker
            .launch(&id)
            .map(Some)
            .map_err(|_| LedgerError::Invalid)
    }
}

struct PlatformObservationRecord {
    state: PlatformAdmissionState,
    binding: Option<WholeCodeBinding>,
    broker: Option<CodePlatformBinding>,
    result: Option<String>,
}
fn verify_unchanged_platform_owner(
    current: &OwnedCodePlatformRuntime,
    prior: &OwnedCodePlatformRuntime,
) -> Result<(), LedgerError> {
    if current != prior {
        return Err(LedgerError::Fenced);
    }
    Ok(())
}
fn completed_observation(
    state: Option<&PlatformAdmissionState>,
    expected_digest: &[u8; 32],
    permitted: bool,
    result: Option<&str>,
) -> Result<bool, LedgerError> {
    let Some(state) = state else { return Ok(false) };
    if state.digest.as_slice() != expected_digest {
        return Err(LedgerError::Conflict);
    }
    if state.phase != Phase::Completed {
        return Ok(false);
    }
    if state.cancelled || !state.was_dispatched || !permitted {
        return Err(LedgerError::Fenced);
    }
    if !result.is_some_and(|result| whole_result(result.as_bytes())) {
        return Err(LedgerError::Invalid);
    }
    Ok(true)
}
#[allow(
    clippy::struct_excessive_bools,
    reason = "Preserve the existing explicit persisted state and authority fields."
)]
struct PlatformAdmissionState {
    digest: Vec<u8>,
    phase: Phase,
    was_dispatched: bool,
    cancelled: bool,
    has_receipt: bool,
    running_owner: bool,
}
fn admission_not_ready(
    state: Option<&PlatformAdmissionState>,
    expected_digest: &[u8; 32],
    permitted: bool,
) -> Result<bool, LedgerError> {
    let Some(state) = state else {
        return if permitted {
            Ok(true)
        } else {
            Err(LedgerError::Fenced)
        };
    };
    if state.digest.as_slice() != expected_digest {
        return Err(LedgerError::Conflict);
    }
    if state.cancelled || state.has_receipt {
        return Err(LedgerError::Fenced);
    }
    match state.phase {
        Phase::Reserved if !state.was_dispatched && permitted => Ok(true),
        Phase::Dispatched if state.was_dispatched && state.running_owner => Ok(false),
        _ => Err(LedgerError::Fenced),
    }
}
#[cfg(test)]
#[path = "ledger_code_platform_not_ready_tests.rs"]
mod not_ready_tests;
