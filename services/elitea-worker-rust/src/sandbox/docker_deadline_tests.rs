//! `PostgreSQL` and runtime-double checks. These tests do not prove deployed runtime behavior.
use super::*;
use crate::state::postgres_session_tests::IsolatedPostgres;
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, docker::CodeJobIdentity},
};
use chrono::{DateTime, Utc};
use sqlx::PgPool;
use std::sync::{
    Arc,
    atomic::{AtomicBool, AtomicUsize, Ordering},
};
use tokio::sync::Notify;

const OWNER: &str = "deadline-owner";
const RUNTIME_ID: &str = "original-runtime";
const DATABASE_URL: &str = "ELITEA_TEST_DATABASE_URL";
const PHASE_MIGRATION: &str =
    include_str!("../../../elitea-main/migrations/agentstate/0010_sandbox_phase_deadlines.sql");
const WAIT: Duration = Duration::from_secs(10);

async fn database(with_phase_clocks: bool) -> IsolatedPostgres {
    let database = IsolatedPostgres::create(
        &std::env::var(DATABASE_URL).expect("ELITEA_TEST_DATABASE_URL is required"),
    )
    .await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&database.pool)
        .await
        .unwrap();
    for migration in [
        include_str!("../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"),
        include_str!("../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0006_sandbox_stop_reconciliation.sql"
        ),
        include_str!("../../../elitea-main/migrations/agentstate/0008_sandbox_runtime_binding.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0009_sandbox_preparation_bundle.sql"
        ),
    ] {
        sqlx::raw_sql(migration)
            .execute(&database.pool)
            .await
            .unwrap();
    }
    if with_phase_clocks {
        sqlx::raw_sql(PHASE_MIGRATION)
            .execute(&database.pool)
            .await
            .unwrap();
    }
    database
}

fn request() -> PreparedJob {
    PreparedJob::new(
        Language::Python,
        "print(42)".into(),
        std::collections::BTreeMap::new(),
        format!("sha256:{}", "a".repeat(64)),
        "deadline-test-v1".into(),
        30,
    )
    .unwrap()
}

fn scope(request: &PreparedJob, key: u8) -> JobScope {
    request
        .scope("deadline-tenant".into(), 2, [key; 32])
        .unwrap()
}

async fn timestamps(pool: &PgPool, key: u8) -> (Option<DateTime<Utc>>, Option<DateTime<Utc>>) {
    sqlx::query_as(
        "SELECT runtime_bound_at,dispatched_at FROM elitea_runtime.sandbox_jobs WHERE job_key=$1",
    )
    .bind([key; 32].as_slice())
    .fetch_one(pool)
    .await
    .unwrap()
}

async fn expire_lease(pool: &PgPool, key: u8) {
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE job_key=$1")
        .bind([key; 32].as_slice()).execute(pool).await.unwrap();
}

async fn claim(ledger: &JobLedger, scope: &JobScope, owner: &str) -> JobLease {
    ledger
        .claim(scope, owner.into(), LEASE_SECONDS)
        .await
        .unwrap()
        .unwrap()
}

fn terminal(result: Result<Reconciliation, SupervisorError>) -> JobRecord {
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = result.unwrap()
    else {
        panic!("The original runtime must have a terminal record")
    };
    assert!(!cleanup_pending);
    assert_eq!(record.runtime_id.as_deref(), Some(RUNTIME_ID));
    assert!(record.result_json.is_none());
    record
}

#[derive(Clone)]
struct Runtime(Arc<RuntimeState>);

struct RuntimeState {
    identity: CodeJobIdentity,
    exists: AtomicBool,
    ready: AtomicBool,
    prepares: AtomicUsize,
    readiness: AtomicUsize,
    dispatches: AtomicUsize,
    receipts: AtomicUsize,
    terminations: AtomicUsize,
    cleanups: AtomicUsize,
    observed: Notify,
}

impl Runtime {
    fn new(scope: &JobScope) -> Self {
        Self(Arc::new(RuntimeState {
            identity: scope.runtime_identity().unwrap(),
            exists: AtomicBool::new(false),
            ready: AtomicBool::new(false),
            prepares: AtomicUsize::new(0),
            readiness: AtomicUsize::new(0),
            dispatches: AtomicUsize::new(0),
            receipts: AtomicUsize::new(0),
            terminations: AtomicUsize::new(0),
            cleanups: AtomicUsize::new(0),
            observed: Notify::new(),
        }))
    }

    fn assert_identity(&self, identity: &CodeJobIdentity, bound: bool) {
        assert_eq!(identity.job_key(), self.0.identity.job_key());
        assert_eq!(identity.request_digest(), self.0.identity.request_digest());
        if bound || identity.runtime_id().is_some() {
            assert_eq!(identity.runtime_id(), Some(RUNTIME_ID));
        }
    }

    async fn wait_for(&self, counter: &AtomicUsize, count: usize) {
        tokio::time::timeout(WAIT, async {
            loop {
                let observed = self.0.observed.notified();
                if counter.load(Ordering::SeqCst) >= count {
                    return;
                }
                observed.await;
            }
        })
        .await
        .expect("runtime observation must arrive");
    }

    fn assert_counts(&self, prepares: usize, dispatches: usize, terminations: usize) {
        assert_eq!(self.0.prepares.load(Ordering::SeqCst), prepares);
        assert_eq!(self.0.dispatches.load(Ordering::SeqCst), dispatches);
        assert_eq!(self.0.terminations.load(Ordering::SeqCst), terminations);
        assert_eq!(self.0.cleanups.load(Ordering::SeqCst), terminations);
    }
}

#[async_trait::async_trait]
impl CodeJobRuntime for Runtime {
    fn image_digest(&self) -> &'static str {
        "unused-by-private-lifecycle-test"
    }
    fn code_compilation_enabled(&self) -> bool {
        false
    }
    fn code_job_timeout(&self) -> Duration {
        Duration::from_secs(30)
    }

    async fn instance(&self, identity: &CodeJobIdentity) -> Result<Option<String>, SandboxError> {
        self.assert_identity(identity, false);
        assert!(self.0.exists.load(Ordering::SeqCst));
        Ok(Some(RUNTIME_ID.into()))
    }

    async fn exists(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        self.assert_identity(identity, false);
        Ok(self.0.exists.load(Ordering::SeqCst))
    }

    async fn prepare(
        &self,
        identity: &CodeJobIdentity,
        _manifest: &Manifest,
    ) -> Result<(), SandboxError> {
        self.assert_identity(identity, false);
        assert!(
            !self.0.exists.swap(true, Ordering::SeqCst),
            "Do not replace the original runtime"
        );
        self.0.prepares.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }

    async fn prepared(&self, identity: &CodeJobIdentity) -> Result<bool, SandboxError> {
        self.assert_identity(identity, true);
        let ready = self.0.ready.load(Ordering::SeqCst);
        self.0.readiness.fetch_add(1, Ordering::SeqCst);
        self.0.observed.notify_one();
        Ok(ready)
    }

    async fn dispatch(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.assert_identity(identity, true);
        self.0.dispatches.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }

    async fn receipt(&self, identity: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        self.assert_identity(identity, true);
        self.0.receipts.fetch_add(1, Ordering::SeqCst);
        self.0.observed.notify_one();
        Ok(None)
    }

    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.assert_identity(identity, true);
        self.0.terminations.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }

    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        self.assert_identity(identity, true);
        self.0.cleanups.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn phase_deadline_slow_reservation_keeps_fresh_readiness_and_execution_budgets() {
    let database = database(true).await;
    let request = request();
    let scope = scope(&request, 51);
    let runtime = Runtime::new(&scope);
    let supervisor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap();
    supervisor.ledger.reserve(&scope).await.unwrap();
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET created_at=clock_timestamp()-interval '2 hours'",
    )
    .execute(&database.pool)
    .await
    .unwrap();
    let lease = claim(&supervisor.ledger, &scope, OWNER).await;
    let progress = async {
        runtime.wait_for(&runtime.0.readiness, 1).await;
        let bound = timestamps(&database.pool, 51).await;
        assert!(bound.0.is_some());
        assert!(bound.1.is_none());
        assert!(
            supervisor
                .ledger
                .readiness_age_seconds(&scope)
                .await
                .unwrap()
                < 60
        );
        runtime.0.ready.store(true, Ordering::SeqCst);
        // The second observation proves the first absent receipt passed the execution deadline check.
        runtime.wait_for(&runtime.0.receipts, 2).await;
        let dispatched = timestamps(&database.pool, 51).await;
        assert_eq!(dispatched.0, bound.0);
        assert!(dispatched.1.is_some());
        assert!(
            supervisor
                .ledger
                .execution_age_seconds(&scope)
                .await
                .unwrap()
                < MAX_JOB_AGE_SECONDS
        );
        supervisor
            .ledger
            .request_cancellation(&scope, OWNER)
            .await
            .unwrap();
        dispatched
    };
    let (result, dispatched) = tokio::time::timeout(WAIT, async {
        tokio::join!(
            supervisor.run_owned(&scope, &lease, Some(&request)),
            progress
        )
    })
    .await
    .expect("fresh phase observation and cancellation must finish");
    assert_eq!(terminal(result).phase, Phase::Cancelled);
    assert_eq!(timestamps(&database.pool, 51).await, dispatched);
    runtime.assert_counts(1, 1, 1);
    database.pool.close().await;
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn phase_deadline_timestamps_survive_binding_renewal_fencing_and_replacement() {
    let database = database(true).await;
    let request = request();
    let scope = scope(&request, 52);
    let ledger = JobLedger::new(database.pool.clone());
    ledger.reserve(&scope).await.unwrap();
    let first = claim(&ledger, &scope, "first-owner").await;
    ledger.bind_runtime(&first, RUNTIME_ID).await.unwrap();
    let bound = timestamps(&database.pool, 52).await;
    assert!(bound.0.is_some());
    assert!(bound.1.is_none());
    ledger.bind_runtime(&first, RUNTIME_ID).await.unwrap();
    ledger.renew(&first, LEASE_SECONDS).await.unwrap();
    assert_eq!(timestamps(&database.pool, 52).await, bound);
    expire_lease(&database.pool, 52).await;
    let next = claim(&ledger, &scope, "next-owner").await;
    assert!(matches!(
        ledger.bind_runtime(&first, RUNTIME_ID).await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        ledger.mark_dispatched(&first).await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        ledger.renew(&first, LEASE_SECONDS).await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        ledger.bind_runtime(&next, "replacement-runtime").await,
        Err(LedgerError::Fenced)
    ));
    ledger.bind_runtime(&next, RUNTIME_ID).await.unwrap();
    assert_eq!(timestamps(&database.pool, 52).await, bound);
    ledger.mark_dispatched(&next).await.unwrap();
    let dispatched = timestamps(&database.pool, 52).await;
    assert_eq!(dispatched.0, bound.0);
    assert!(dispatched.1.is_some());
    assert!(matches!(
        ledger.mark_dispatched(&next).await,
        Err(LedgerError::Fenced)
    ));
    expire_lease(&database.pool, 52).await;
    let replacement = claim(&ledger, &scope, "replacement-owner").await;
    assert_eq!(replacement.observed_phase, Phase::Dispatched);
    ledger.renew(&replacement, LEASE_SECONDS).await.unwrap();
    assert_eq!(timestamps(&database.pool, 52).await, dispatched);
    assert_eq!(
        ledger.read(&scope).await.unwrap().runtime_id.as_deref(),
        Some(RUNTIME_ID)
    );
    database.pool.close().await;
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn phase_deadline_expired_readiness_stops_before_dispatch_after_takeover() {
    let database = database(true).await;
    let request = request();
    let scope = scope(&request, 53);
    let runtime = Runtime::new(&scope);
    let supervisor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap();
    supervisor.ledger.reserve(&scope).await.unwrap();
    let first = claim(&supervisor.ledger, &scope, "first-owner").await;
    runtime
        .prepare(
            &scope.runtime_identity().unwrap(),
            &request.manifest().unwrap(),
        )
        .await
        .unwrap();
    supervisor
        .ledger
        .bind_runtime(&first, RUNTIME_ID)
        .await
        .unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET runtime_bound_at=clock_timestamp()-interval '61 seconds'")
        .execute(&database.pool).await.unwrap();
    let bound = timestamps(&database.pool, 53).await;
    expire_lease(&database.pool, 53).await;
    let replacement = claim(&supervisor.ledger, &scope, OWNER).await;
    let record = terminal(
        tokio::time::timeout(
            WAIT,
            supervisor.run_owned(&scope, &replacement, Some(&request)),
        )
        .await
        .expect("expired readiness must finish"),
    );
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(
        record.failure_code.as_deref(),
        Some("sandbox.preparation_incomplete")
    );
    assert_eq!(timestamps(&database.pool, 53).await, bound);
    assert!(bound.1.is_none());
    assert_eq!(runtime.0.receipts.load(Ordering::SeqCst), 0);
    runtime.assert_counts(1, 0, 1);
    database.pool.close().await;
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn phase_deadline_expired_execution_stops_before_recovery_dispatch() {
    let database = database(true).await;
    let request = request();
    let scope = scope(&request, 54);
    let runtime = Runtime::new(&scope);
    let supervisor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap();
    supervisor.ledger.reserve(&scope).await.unwrap();
    let first = claim(&supervisor.ledger, &scope, "first-owner").await;
    runtime
        .prepare(
            &scope.runtime_identity().unwrap(),
            &request.manifest().unwrap(),
        )
        .await
        .unwrap();
    supervisor
        .ledger
        .bind_runtime(&first, RUNTIME_ID)
        .await
        .unwrap();
    supervisor.ledger.mark_dispatched(&first).await.unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET dispatched_at=clock_timestamp()-interval '3661 seconds'")
        .execute(&database.pool).await.unwrap();
    let dispatched = timestamps(&database.pool, 54).await;
    expire_lease(&database.pool, 54).await;
    let record = terminal(
        tokio::time::timeout(WAIT, supervisor.reconcile_dispatched(&scope))
            .await
            .expect("expired execution must finish"),
    );
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(
        record.failure_code.as_deref(),
        Some("sandbox.deadline_exceeded")
    );
    assert_eq!(timestamps(&database.pool, 54).await, dispatched);
    assert_eq!(runtime.0.receipts.load(Ordering::SeqCst), 1);
    runtime.assert_counts(1, 0, 1);
    database.pool.close().await;
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn phase_deadline_cancellation_during_readiness_keeps_dispatch_timestamp_absent() {
    let database = database(true).await;
    let request = request();
    let scope = scope(&request, 55);
    let runtime = Runtime::new(&scope);
    let supervisor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap();
    supervisor.ledger.reserve(&scope).await.unwrap();
    let lease = claim(&supervisor.ledger, &scope, OWNER).await;
    let cancel = async {
        runtime.wait_for(&runtime.0.readiness, 1).await;
        let bound = timestamps(&database.pool, 55).await;
        assert!(bound.0.is_some());
        assert!(bound.1.is_none());
        supervisor
            .ledger
            .request_cancellation(&scope, OWNER)
            .await
            .unwrap();
        assert!(matches!(
            supervisor.ledger.mark_dispatched(&lease).await,
            Err(LedgerError::Fenced)
        ));
        bound
    };
    let (result, bound) = tokio::time::timeout(WAIT, async {
        tokio::join!(supervisor.run_owned(&scope, &lease, Some(&request)), cancel)
    })
    .await
    .expect("readiness cancellation must finish");
    let record = terminal(result);
    assert_eq!(record.phase, Phase::Cancelled);
    assert_eq!(record.failure_code.as_deref(), Some("sandbox.cancelled"));
    assert_eq!(timestamps(&database.pool, 55).await, bound);
    assert_eq!(runtime.0.receipts.load(Ordering::SeqCst), 0);
    runtime.assert_counts(1, 0, 1);
    database.pool.close().await;
}

async fn insert_old_writer(
    pool: &PgPool,
    request: &PreparedJob,
    key: u8,
    phase: &str,
    runtime_id: Option<&str>,
) {
    // The old writer omits both phase clocks, before and after the migration.
    sqlx::query("INSERT INTO elitea_runtime.sandbox_jobs (tenant_id,project_id,job_key,request_digest,phase,runtime_id,created_at) VALUES ('deadline-tenant',2,$1,$2,$3,$4,clock_timestamp()-interval '2 hours')")
        .bind([key; 32].as_slice()).bind(request.fingerprint().unwrap().as_slice())
        .bind(phase).bind(runtime_id).execute(pool).await.unwrap();
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
#[expect(
    clippy::too_many_lines,
    reason = "Keep migration and rolling writer compatibility in one ordered lifecycle"
)]
async fn phase_deadline_migration_and_old_writer_preserve_conservative_legacy_budgets() {
    let database = database(false).await;
    let request = request();
    insert_old_writer(&database.pool, &request, 60, "dispatched", Some(RUNTIME_ID)).await;
    insert_old_writer(&database.pool, &request, 61, "reserved", Some(RUNTIME_ID)).await;
    insert_old_writer(&database.pool, &request, 62, "reserved", None).await;
    sqlx::raw_sql(PHASE_MIGRATION)
        .execute(&database.pool)
        .await
        .unwrap();
    let backfill: Vec<(bool, bool)> = sqlx::query_as("SELECT runtime_bound_at IS NOT DISTINCT FROM CASE WHEN runtime_id IS NOT NULL THEN created_at END,dispatched_at IS NOT DISTINCT FROM CASE WHEN phase <> 'reserved' THEN created_at END FROM elitea_runtime.sandbox_jobs ORDER BY job_key")
        .fetch_all(&database.pool).await.unwrap();
    assert_eq!(backfill, vec![(true, true); 3]);
    let ledger = JobLedger::new(database.pool.clone());
    assert!(
        ledger
            .readiness_age_seconds(&scope(&request, 60))
            .await
            .unwrap()
            >= 7200
    );
    assert!(
        ledger
            .execution_age_seconds(&scope(&request, 60))
            .await
            .unwrap()
            >= 7200
    );
    assert!(
        ledger
            .readiness_age_seconds(&scope(&request, 61))
            .await
            .unwrap()
            >= 7200
    );
    // An unbound reservation gets a fresh clock only when the new writer binds its runtime.
    let unbound_scope = scope(&request, 62);
    let unbound = claim(&ledger, &unbound_scope, OWNER).await;
    ledger.bind_runtime(&unbound, RUNTIME_ID).await.unwrap();
    assert!(ledger.readiness_age_seconds(&unbound_scope).await.unwrap() < 60);

    insert_old_writer(&database.pool, &request, 63, "reserved", Some(RUNTIME_ID)).await;
    insert_old_writer(&database.pool, &request, 64, "dispatched", Some(RUNTIME_ID)).await;
    assert_eq!(timestamps(&database.pool, 63).await, (None, None));
    assert_eq!(timestamps(&database.pool, 64).await, (None, None));
    let old_bound_scope = scope(&request, 63);
    let old_dispatch_scope = scope(&request, 64);
    assert!(
        ledger
            .readiness_age_seconds(&old_bound_scope)
            .await
            .unwrap()
            >= 7200
    );
    assert!(
        ledger
            .execution_age_seconds(&old_dispatch_scope)
            .await
            .unwrap()
            >= 7200
    );

    let runtime = Runtime::new(&old_bound_scope);
    runtime.0.exists.store(true, Ordering::SeqCst);
    let supervisor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap();
    let takeover = claim(&supervisor.ledger, &old_bound_scope, OWNER).await;
    let record = terminal(
        tokio::time::timeout(
            WAIT,
            supervisor.run_owned(&old_bound_scope, &takeover, Some(&request)),
        )
        .await
        .expect("legacy readiness takeover must retain its expired budget"),
    );
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(
        record.failure_code.as_deref(),
        Some("sandbox.preparation_incomplete")
    );
    let inherited: bool = sqlx::query_scalar("SELECT runtime_bound_at=created_at AND dispatched_at IS NULL FROM elitea_runtime.sandbox_jobs WHERE job_key=$1")
        .bind([63_u8; 32].as_slice()).fetch_one(&database.pool).await.unwrap();
    assert!(inherited);
    runtime.assert_counts(0, 0, 1);

    let runtime = Runtime::new(&old_dispatch_scope);
    runtime.0.exists.store(true, Ordering::SeqCst);
    let supervisor = DockerSupervisor::new(
        JobLedger::new(database.pool.clone()),
        runtime.clone(),
        OWNER.into(),
        1,
    )
    .unwrap();
    let record = terminal(
        tokio::time::timeout(WAIT, supervisor.reconcile_dispatched(&old_dispatch_scope))
            .await
            .expect("legacy execution fallback must expire before recovery dispatch"),
    );
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(
        record.failure_code.as_deref(),
        Some("sandbox.deadline_exceeded")
    );
    assert_eq!(timestamps(&database.pool, 64).await, (None, None));
    assert_eq!(runtime.0.receipts.load(Ordering::SeqCst), 1);
    runtime.assert_counts(0, 0, 1);
    database.pool.close().await;
}
