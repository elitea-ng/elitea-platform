use sqlx::{PgPool, Row};

use super::dependency_content::PythonDependencyBundle;

/// Construct only from an authenticated invocation, never caller-selected tenant headers.
#[derive(Clone)]
pub struct JobScope {
    tenant: String,
    project: i32,
    key: [u8; 32],
    digest: [u8; 32],
}

impl JobScope {
    /// # Errors
    /// Returns `Invalid` for an invalid tenant or project identity.
    pub fn new(
        tenant: String,
        project: i32,
        key: [u8; 32],
        digest: [u8; 32],
    ) -> Result<Self, LedgerError> {
        if tenant.is_empty() || tenant.len() > 256 || tenant.contains('\0') || project <= 0 {
            return Err(LedgerError::Invalid);
        }
        Ok(Self {
            tenant,
            project,
            key,
            digest,
        })
    }

    /// Names are globally unique within a runtime even when callers reuse an
    /// execution-local key across projects. No tenant text enters runtime metadata.
    /// # Errors
    /// Returns `Invalid` if runtime identity construction fails.
    pub fn runtime_identity(
        &self,
    ) -> Result<adk_sandbox::workspace::docker::CodeJobIdentity, LedgerError> {
        let mut hash = ring::digest::Context::new(&ring::digest::SHA256);
        hash.update(b"elitea.sandbox.runtime-job.v1\0");
        hash.update(&(self.tenant.len() as u64).to_be_bytes());
        hash.update(self.tenant.as_bytes());
        hash.update(&self.project.to_be_bytes());
        hash.update(&self.key);
        adk_sandbox::workspace::docker::CodeJobIdentity::new(
            hex(hash.finish().as_ref()),
            hex(&self.digest),
        )
        .map_err(|_| LedgerError::Invalid)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Reserved,
    Dispatched,
    Completed,
    Failed,
    Cancelled,
    Uncertain,
}

impl Phase {
    fn parse(value: &str) -> Result<Self, LedgerError> {
        match value {
            "reserved" => Ok(Self::Reserved),
            "dispatched" => Ok(Self::Dispatched),
            "completed" => Ok(Self::Completed),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            "uncertain" => Ok(Self::Uncertain),
            _ => Err(LedgerError::Invalid),
        }
    }
}

/// No Debug implementation: the result may contain user data.
pub struct JobRecord {
    pub phase: Phase,
    pub result_json: Option<String>,
    pub failure_code: Option<String>,
    pub runtime_id: Option<String>,
}

pub struct JobLease {
    scope: JobScope,
    owner: String,
    epoch: i64,
    pub observed_phase: Phase,
}

#[derive(Debug, thiserror::Error)]
pub enum LedgerError {
    #[error("sandbox job identity or receipt is invalid")]
    Invalid,
    #[error("sandbox job conflicts with a different request")]
    Conflict,
    #[error("sandbox job is missing")]
    Missing,
    #[error("sandbox job ownership expired or the state already advanced")]
    Fenced,
    #[error("sandbox job persistence failed")]
    Database(#[source] sqlx::Error),
}
impl From<sqlx::Error> for LedgerError {
    fn from(error: sqlx::Error) -> Self {
        Self::Database(error)
    }
}

pub struct JobLedger {
    pool: PgPool,
}

impl JobLedger {
    #[must_use]
    pub fn new(pool: PgPool) -> Self {
        Self { pool }
    }

    /// Insert once. Repeated submissions read the existing terminal or in-flight receipt.
    /// # Errors
    /// Returns a database error or a conflict with an existing request digest.
    pub async fn reserve(&self, scope: &JobScope) -> Result<JobRecord, LedgerError> {
        sqlx::query("INSERT INTO elitea_runtime.sandbox_jobs (tenant_id,project_id,job_key,request_digest) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice())
            .execute(&self.pool).await?;
        self.read(scope).await
    }

    /// # Errors
    /// Returns `Missing`, `Conflict`, or a database/record decoding error.
    pub async fn read(&self, scope: &JobScope) -> Result<JobRecord, LedgerError> {
        let row = sqlx::query("SELECT request_digest,phase,result_json,failure_code,runtime_id FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice())
            .fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)?;
        let digest: Vec<u8> = row.try_get("request_digest")?;
        if digest.as_slice() != scope.digest {
            return Err(LedgerError::Conflict);
        }
        Ok(JobRecord {
            phase: Phase::parse(row.try_get("phase")?)?,
            result_json: row.try_get("result_json")?,
            failure_code: row.try_get("failure_code")?,
            runtime_id: row.try_get("runtime_id")?,
        })
    }

    /// Read immutable preparation metadata after owner replacement or terminal completion.
    /// The recorded database digest supplies the bundle validation root.
    /// # Errors
    /// Returns `Missing`, `Conflict`, `Invalid`, or a database error.
    pub async fn read_preparation_bundle(
        &self,
        scope: &JobScope,
    ) -> Result<Option<PythonDependencyBundle>, LedgerError> {
        let row = sqlx::query("SELECT request_digest,preparation_bundle_json FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice())
            .fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)?;
        let digest: Vec<u8> = row.try_get("request_digest")?;
        if digest.as_slice() != scope.digest {
            return Err(LedgerError::Conflict);
        }
        let record: Option<String> = row.try_get("preparation_bundle_json")?;
        record
            .map(|record| {
                #[derive(serde::Deserialize)]
                struct RecordedRoot {
                    digest: String,
                }
                let root: RecordedRoot =
                    serde_json::from_str(&record).map_err(|_| LedgerError::Invalid)?;
                PythonDependencyBundle::parse(record.as_bytes(), &root.digest)
                    .map_err(|_| LedgerError::Invalid)
            })
            .transpose()
    }

    /// Record validated metadata before shared publication. Identical writes are idempotent.
    /// A live dispatched lease admits one immutable bundle while cancellation remains absent.
    /// # Errors
    /// Returns `Conflict` for changed metadata, `Fenced` for lost authority, or a database error.
    pub async fn record_preparation_bundle(
        &self,
        lease: &JobLease,
        bundle: &PythonDependencyBundle,
    ) -> Result<(), LedgerError> {
        let record = std::str::from_utf8(bundle.record_json()).map_err(|_| LedgerError::Invalid)?;
        let mut transaction = self.pool.begin().await?;
        // Check database time after locking. A lock wait can outlive the lease.
        let row = sqlx::query("WITH owned AS MATERIALIZED (SELECT preparation_bundle_json,lease_until FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND phase='dispatched' AND NOT cancellation_requested FOR UPDATE) SELECT preparation_bundle_json FROM owned WHERE lease_until > clock_timestamp()")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice())
            .bind(&lease.owner).bind(lease.epoch).fetch_optional(&mut *transaction).await?
            .ok_or(LedgerError::Fenced)?;
        let recorded: Option<String> = row.try_get("preparation_bundle_json")?;
        if let Some(recorded) = recorded {
            if recorded != record {
                return Err(LedgerError::Conflict);
            }
        } else {
            let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET preparation_bundle_json=$7,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND phase='dispatched' AND NOT cancellation_requested AND preparation_bundle_json IS NULL")
                .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice())
                .bind(&lease.owner).bind(lease.epoch).bind(record).execute(&mut *transaction).await?.rows_affected();
            changed(count)?;
        }
        transaction.commit().await?;
        Ok(())
    }

    /// Start readiness at the first runtime binding. Lease renewal does not reset it.
    /// # Errors
    /// Returns `Missing` for an unknown request identity or a database error.
    pub async fn readiness_age_seconds(&self, scope: &JobScope) -> Result<i64, LedgerError> {
        sqlx::query_scalar("SELECT GREATEST(0, EXTRACT(EPOCH FROM (clock_timestamp()-COALESCE(runtime_bound_at,created_at)))::bigint) FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice())
            .fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)
    }

    /// Start execution observation at dispatch. Reconnects and lease renewal do not reset it.
    /// # Errors
    /// Returns `Missing` for an unknown request identity or a database error.
    pub async fn execution_age_seconds(&self, scope: &JobScope) -> Result<i64, LedgerError> {
        sqlx::query_scalar("SELECT GREATEST(0, EXTRACT(EPOCH FROM (clock_timestamp()-COALESCE(dispatched_at,created_at)))::bigint) FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice())
            .fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)
    }

    /// Persist an authenticated stop request without claiming that the runtime stopped.
    /// The caller must authorize this exact scope before calling this method.
    /// A missing job is reserved to prevent a later submission from starting it.
    /// # Errors
    /// Returns an identity conflict or database error.
    pub async fn request_cancellation(
        &self,
        scope: &JobScope,
        cancellation_owner: &str,
    ) -> Result<JobRecord, LedgerError> {
        if cancellation_owner.is_empty()
            || cancellation_owner.len() > 128
            || cancellation_owner.contains('\0')
        {
            return Err(LedgerError::Invalid);
        }
        self.reserve(scope).await?;
        sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET cancellation_requested=TRUE,cancellation_owner=COALESCE(cancellation_owner,$5),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase IN ('reserved','dispatched') AND (NOT cancellation_requested OR cancellation_owner IS NULL)")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).bind(cancellation_owner)
            .execute(&self.pool).await?;
        self.read(scope).await
    }

    /// # Errors
    /// Returns `Missing` for an unknown scope or a database error.
    pub async fn cancellation_requested(&self, scope: &JobScope) -> Result<bool, LedgerError> {
        sqlx::query_scalar("SELECT cancellation_requested FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice())
            .fetch_optional(&self.pool).await?.ok_or(LedgerError::Missing)
    }

    /// Bounded recovery of stop intents admitted by this stable supervisor owner.
    /// This discovers no executable work and never authorizes a new dispatch.
    /// # Errors
    /// Returns invalid bounds, corrupt identities, or a database error.
    pub async fn pending_cancellations(
        &self,
        owner: &str,
        limit: i64,
    ) -> Result<Vec<JobScope>, LedgerError> {
        if owner.is_empty()
            || owner.len() > 128
            || owner.contains('\0')
            || !(1..=32).contains(&limit)
        {
            return Err(LedgerError::Invalid);
        }
        let rows = sqlx::query("SELECT tenant_id,project_id,job_key,request_digest FROM elitea_runtime.sandbox_jobs WHERE cancellation_owner=$1 AND cancellation_requested AND phase IN ('reserved','dispatched') AND (lease_until IS NULL OR lease_until <= clock_timestamp()) ORDER BY updated_at,tenant_id,project_id,job_key LIMIT $2")
            .bind(owner).bind(limit).fetch_all(&self.pool).await?;
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

    /// Reclaiming a dispatched job grants reconciliation, never permission to replay.
    /// Database time and a monotonically increasing epoch fence previous owners.
    /// # Errors
    /// Returns an invalid input, identity conflict, or database error.
    pub async fn claim(
        &self,
        scope: &JobScope,
        owner: String,
        ttl_seconds: i32,
    ) -> Result<Option<JobLease>, LedgerError> {
        validate_ttl(ttl_seconds)?;
        if owner.is_empty() || owner.len() > 128 || owner.contains('\0') {
            return Err(LedgerError::Invalid);
        }
        self.read(scope).await?;
        let row = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET owner_id=$5,lease_epoch=lease_epoch+1,lease_until=clock_timestamp()+make_interval(secs => $6),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND phase IN ('reserved','dispatched') AND (lease_until IS NULL OR lease_until <= clock_timestamp()) RETURNING lease_epoch,phase")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice())
            .bind(&owner).bind(f64::from(ttl_seconds)).fetch_optional(&self.pool).await?;
        row.map(|row| {
            Ok(JobLease {
                scope: scope.clone(),
                owner,
                epoch: row.try_get("lease_epoch")?,
                observed_phase: Phase::parse(row.try_get("phase")?)?,
            })
        })
        .transpose()
    }

    /// A durable, authenticated stop supersedes dispatch authority immediately.
    /// This lease cannot admit execution: the stop flag is irreversible and both
    /// dispatch and successful completion reject it. Old lease epochs are fenced.
    /// A live preparation lease must first drain: a provision RPC may still create
    /// its container after an early absence observation.
    pub(crate) async fn claim_cancellation(
        &self,
        scope: &JobScope,
        owner: &str,
        ttl_seconds: i32,
    ) -> Result<Option<JobLease>, LedgerError> {
        validate_ttl(ttl_seconds)?;
        if owner.is_empty() || owner.len() > 128 || owner.contains('\0') {
            return Err(LedgerError::Invalid);
        }
        self.read(scope).await?;
        let row = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET owner_id=$5,lease_epoch=lease_epoch+1,lease_until=clock_timestamp()+make_interval(secs => $6),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND cancellation_requested AND phase IN ('reserved','dispatched') AND (phase='dispatched' OR lease_until IS NULL OR lease_until <= clock_timestamp()) RETURNING lease_epoch,phase")
            .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice())
            .bind(owner).bind(f64::from(ttl_seconds)).fetch_optional(&self.pool).await?;
        row.map(|row| {
            Ok(JobLease {
                scope: scope.clone(),
                owner: owner.to_owned(),
                epoch: row.try_get("lease_epoch")?,
                observed_phase: Phase::parse(row.try_get("phase")?)?,
            })
        })
        .transpose()
    }

    /// # Errors
    /// Returns `Invalid`, `Fenced`, or a database error.
    pub async fn renew(&self, lease: &JobLease, ttl_seconds: i32) -> Result<(), LedgerError> {
        validate_ttl(ttl_seconds)?;
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()+make_interval(secs => $7),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND phase IN ('reserved','dispatched')")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice())
            .bind(&lease.owner).bind(lease.epoch).bind(f64::from(ttl_seconds)).execute(&self.pool).await?.rows_affected();
        changed(count)
    }

    /// Bind once before dispatch. Repeating the same binding is idempotent;
    /// changing it or using an expired lease is rejected.
    /// Preserve reservation time when an older supervisor already bound the runtime.
    /// # Errors
    /// Returns `Invalid`, `Fenced`, or a database error.
    pub async fn bind_runtime(
        &self,
        lease: &JobLease,
        runtime_id: &str,
    ) -> Result<(), LedgerError> {
        if runtime_id.is_empty()
            || runtime_id.len() > 512
            || runtime_id
                .chars()
                .any(|c| c.is_whitespace() || c.is_control())
        {
            return Err(LedgerError::Invalid);
        }
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET runtime_id=$7,runtime_bound_at=COALESCE(runtime_bound_at,CASE WHEN runtime_id IS NULL THEN clock_timestamp() ELSE created_at END),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND phase='reserved' AND (runtime_id IS NULL OR runtime_id=$7)")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice())
            .bind(&lease.owner).bind(lease.epoch).bind(runtime_id).execute(&self.pool).await?.rows_affected();
        changed(count)
    }

    /// Persist before launching code. Exactly one caller can cross this boundary.
    /// If dispatch acknowledgement is lost, read/reconcile instead of retrying code.
    /// # Errors
    /// Returns `Fenced` if ownership or phase changed, or a database error.
    pub async fn mark_dispatched(&self, lease: &JobLease) -> Result<(), LedgerError> {
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET phase='dispatched',dispatched_at=COALESCE(dispatched_at,clock_timestamp()),updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND phase='reserved' AND NOT cancellation_requested")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice())
            .bind(&lease.owner).bind(lease.epoch).execute(&self.pool).await?.rows_affected();
        changed(count)
    }

    /// Terminal rows are immutable. A lost finish acknowledgement is recovered by `read()`.
    /// # Errors
    /// Returns an invalid receipt, `Fenced`, or a database error.
    pub async fn finish(
        &self,
        lease: &JobLease,
        phase: Phase,
        result: Option<&str>,
        failure: Option<&str>,
    ) -> Result<(), LedgerError> {
        let phase = match phase {
            Phase::Completed => {
                let result = result.ok_or(LedgerError::Invalid)?;
                if result.len() > 524_288
                    || failure.is_some()
                    || serde_json::from_str::<serde_json::Value>(result).is_err()
                {
                    return Err(LedgerError::Invalid);
                }
                "completed"
            }
            Phase::Failed | Phase::Cancelled | Phase::Uncertain => {
                let code = failure.ok_or(LedgerError::Invalid)?;
                if result.is_some()
                    || code.is_empty()
                    || code.len() > 64
                    || !code.as_bytes()[0].is_ascii_lowercase()
                    || !code.bytes().all(|b| {
                        b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_' || b == b'.'
                    })
                {
                    return Err(LedgerError::Invalid);
                }
                match phase {
                    Phase::Failed => "failed",
                    Phase::Cancelled => "cancelled",
                    _ => "uncertain",
                }
            }
            Phase::Reserved | Phase::Dispatched => return Err(LedgerError::Invalid),
        };
        let count = sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET phase=$7,result_json=$8,failure_code=$9,updated_at=clock_timestamp() WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4 AND owner_id=$5 AND lease_epoch=$6 AND lease_until > clock_timestamp() AND (phase='dispatched' OR (phase='reserved' AND $7 <> 'completed')) AND (NOT cancellation_requested OR $7='cancelled')")
            .bind(&lease.scope.tenant).bind(lease.scope.project).bind(lease.scope.key.as_slice()).bind(lease.scope.digest.as_slice())
            .bind(&lease.owner).bind(lease.epoch).bind(phase).bind(result).bind(failure).execute(&self.pool).await?.rows_affected();
        changed(count)
    }
}

fn validate_ttl(seconds: i32) -> Result<(), LedgerError> {
    if (5..=300).contains(&seconds) {
        Ok(())
    } else {
        Err(LedgerError::Invalid)
    }
}
fn changed(count: u64) -> Result<(), LedgerError> {
    if count == 1 {
        Ok(())
    } else {
        Err(LedgerError::Fenced)
    }
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(DIGITS[usize::from(byte >> 4)]));
        output.push(char::from(DIGITS[usize::from(byte & 15)]));
    }
    output
}
