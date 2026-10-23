//! Actual owner SQL plus an exact-instance runtime double; not deployment proof.
use super::*;
use crate::{protocol::sandbox_grant::code_owner_fixture, sandbox::runtime::CodeJobRuntime};
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, docker::CodeJobIdentity},
};
use sqlx::Row as _;
use std::sync::{Arc, Mutex};

const INSTANCE: &str = "original-retained-instance";
#[derive(Default)]
struct State {
    running: bool,
    present: bool,
    termination_failures: usize,
    cleanup_failures: usize,
    calls: Vec<&'static str>,
}
#[derive(Clone)]
struct Runtime(Arc<Mutex<State>>);
impl Runtime {
    fn check(identity: &CodeJobIdentity) {
        assert_eq!(identity.runtime_id(), Some(INSTANCE));
    }
}
#[async_trait::async_trait]
impl CodeJobRuntime for Runtime {
    #[allow(
        clippy::unnecessary_literal_bound,
        reason = "Match the existing runtime trait signature in this fixture."
    )]
    fn image_digest(&self) -> &str {
        "unused-cleanup-runtime"
    }
    fn code_compilation_enabled(&self) -> bool {
        false
    }
    fn code_job_timeout(&self) -> Duration {
        Duration::from_secs(30)
    }
    async fn instance(&self, _: &CodeJobIdentity) -> Result<Option<String>, SandboxError> {
        panic!("cleanup selected another runtime")
    }
    async fn exists(&self, _: &CodeJobIdentity) -> Result<bool, SandboxError> {
        panic!("cleanup must use retained instance directly")
    }
    async fn prepare(&self, _: &CodeJobIdentity, _: &Manifest) -> Result<(), SandboxError> {
        panic!("cleanup provisioned a runtime")
    }
    async fn prepared(&self, _: &CodeJobIdentity) -> Result<bool, SandboxError> {
        panic!("cleanup entered execution preparation")
    }
    async fn dispatch(&self, _: &CodeJobIdentity) -> Result<(), SandboxError> {
        panic!("cleanup dispatched Code")
    }
    async fn receipt(&self, _: &CodeJobIdentity) -> Result<Option<Vec<u8>>, SandboxError> {
        panic!("cleanup read business output")
    }
    async fn terminate(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        Self::check(identity);
        let mut state = self.0.lock().unwrap();
        state.calls.push("terminate");
        if state.termination_failures != 0 {
            state.termination_failures -= 1;
            return Err(SandboxError::ExecutionFailed(
                "fixture termination uncertain".into(),
            ));
        }
        state.running = false;
        Ok(())
    }
    async fn cleanup(&self, identity: &CodeJobIdentity) -> Result<(), SandboxError> {
        Self::check(identity);
        let mut state = self.0.lock().unwrap();
        state.calls.push("cleanup");
        assert!(!state.running, "running runtime was removed");
        state.present = false;
        // Simulate removal before an uncertain cleanup response/crash.
        if state.cleanup_failures != 0 {
            state.cleanup_failures -= 1;
            return Err(SandboxError::ExecutionFailed(
                "fixture cleanup uncertain after removal".into(),
            ));
        }
        Ok(())
    }
}
const DATABASE_URL_VAR: &str = "ELITEA_CODE_OWNER_TEST_DATABASE_URL";
async fn ledger() -> JobLedger {
    let url = crate::test_pg::database_url_var(DATABASE_URL_VAR)
        .expect("explicit isolated agentstate database including owner columns required");
    JobLedger::new(
        crate::test_pg::pool_options()
            .max_connections(2)
            .acquire_timeout(Duration::from_secs(5))
            .connect(&url)
            .await
            .unwrap(),
    )
}
/// Deletes every row whose owner starts with this test's owner (its own and
/// any `-other` sibling) when the test ends, including by panic, so a failed
/// run leaves nothing behind in the shared database.
struct OwnerRows(String);
impl Drop for OwnerRows {
    fn drop(&mut self) {
        let owner = self.0.clone();
        // Drop cannot await; a separate thread with its own runtime and
        // connection runs the delete without touching the test's runtime.
        let deleted = std::thread::spawn(move || {
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            runtime.block_on(async {
                let url = crate::test_pg::database_url_var(DATABASE_URL_VAR).unwrap();
                let pool = crate::test_pg::pool_options()
                    .max_connections(1)
                    .acquire_timeout(Duration::from_secs(5))
                    .connect(&url)
                    .await
                    .unwrap();
                sqlx::query(
                    "DELETE FROM elitea_runtime.sandbox_jobs WHERE left(owner_id,length($1))=$1",
                )
                .bind(&owner)
                .execute(&pool)
                .await
                .unwrap();
                pool.close().await;
            });
        })
        .join();
        assert!(
            deleted.is_ok() || std::thread::panicking(),
            "cleanup of the test's owner rows failed"
        );
    }
}
/// One sealed no-effect row and the worker identity that owns it.
struct Sealed {
    fixture: crate::protocol::sandbox_grant::CodeOwnerFixture,
    lease: JobLease,
    owner: String,
    _rows: OwnerRows,
}
/// Seal a row under an owner unique to this call. Reconciliation discovers
/// pending cleanup by `owner_id` alone (a worker reaps everything it owns),
/// so tests sharing an owner reaped each other's rows when run in parallel;
/// the fixture, not each test, therefore chooses the owner.
async fn sealed(ledger: &JobLedger) -> Sealed {
    sealed_as(
        ledger,
        format!("code-no-effect-cleanup-{}", crate::test_pg::unique_hex()),
    )
    .await
}
async fn sealed_as(ledger: &JobLedger, owner: String) -> Sealed {
    let rows = OwnerRows(owner.clone());
    let fixture = code_owner_fixture(crate::test_pg::unique_hex());
    ledger.reserve(&fixture.scope).await.unwrap();
    ledger
        .register_code_intent(&fixture.scope, &fixture.intent)
        .await
        .unwrap();
    let lease = ledger
        .claim(&fixture.scope, owner.clone(), 30)
        .await
        .unwrap()
        .unwrap();
    ledger.bind_runtime(&lease, INSTANCE).await.unwrap();
    let observed = ledger.observe_code_owner(&fixture.seal).await.unwrap();
    assert!(observed.state == crate::sandbox::ledger::CodeOwnerState::VerifiedNoEffect);
    Sealed {
        fixture,
        lease,
        owner,
        _rows: rows,
    }
}
async fn status(ledger: &JobLedger, scope: &JobScope) -> (bool, Option<String>) {
    let row = sqlx::query("SELECT code_recovery_cleanup_at IS NOT NULL AS done,code_recovery_cleanup_failure FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
        .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).fetch_one(&ledger.pool).await.unwrap();
    (
        row.try_get("done").unwrap(),
        row.try_get("code_recovery_cleanup_failure").unwrap(),
    )
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn crash_after_seal_retains_exact_cleanup_authority_without_dispatch() {
    let ledger = ledger().await;
    let job = sealed(&ledger).await;
    let (fixture, old_lease, owner) = (&job.fixture, &job.lease, &job.owner);
    let proof = ledger
        .observe_code_owner(&fixture.read)
        .await
        .unwrap()
        .receipt
        .unwrap()
        .canonical_bytes()
        .unwrap();
    assert!(ledger.mark_dispatched(old_lease).await.is_err());
    // A sibling worker with its own sealed row: discovery is exactly per owner,
    // and this worker's reconcile below must leave the sibling's row alone.
    let sibling = sealed_as(&ledger, format!("{owner}-other")).await;
    let foreign = ledger
        .pending_code_no_effect_cleanup(&sibling.owner, 32)
        .await
        .unwrap();
    assert!(foreign.len() == 1 && foreign[0] == sibling.fixture.scope);
    let own = ledger
        .pending_code_no_effect_cleanup(owner, 32)
        .await
        .unwrap();
    assert!(own.len() == 1 && own[0] == fixture.scope);
    let state = Arc::new(Mutex::new(State {
        running: true,
        present: true,
        ..State::default()
    }));
    let supervisor =
        DockerSupervisor::new(ledger, Runtime(state.clone()), owner.clone(), 1).unwrap();
    supervisor.reconcile_code_no_effect_cleanup().await.unwrap();
    assert_eq!(
        status(&supervisor.ledger, &fixture.scope).await,
        (true, None)
    );
    assert_eq!(state.lock().unwrap().calls, ["terminate", "cleanup"]);
    assert_eq!(
        status(&supervisor.ledger, &sibling.fixture.scope).await,
        (false, None)
    );
    assert!(
        supervisor
            .ledger
            .claim(&fixture.scope, owner.clone(), 30)
            .await
            .unwrap()
            .is_none()
    );
    supervisor.reconcile_code_no_effect_cleanup().await.unwrap();
    assert_eq!(state.lock().unwrap().calls, ["terminate", "cleanup"]);
    assert_eq!(
        supervisor
            .ledger
            .observe_code_owner(&fixture.read)
            .await
            .unwrap()
            .receipt
            .unwrap()
            .canonical_bytes()
            .unwrap(),
        proof
    );
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn uncertain_termination_then_removed_runtime_cleanup_are_durable_and_idempotent() {
    let ledger = ledger().await;
    let job = sealed(&ledger).await;
    let (fixture, _, owner) = (&job.fixture, &job.lease, &job.owner);
    let state = Arc::new(Mutex::new(State {
        running: true,
        present: true,
        termination_failures: 1,
        cleanup_failures: 1,
        ..State::default()
    }));
    let supervisor =
        DockerSupervisor::new(ledger, Runtime(state.clone()), owner.clone(), 1).unwrap();
    assert!(
        supervisor
            .cleanup_code_no_effect_runtime(&fixture.scope)
            .await
            .is_err()
    );
    assert_eq!(
        status(&supervisor.ledger, &fixture.scope).await,
        (false, Some("termination_unconfirmed".into()))
    );
    assert!(state.lock().unwrap().running);
    assert!(
        supervisor
            .cleanup_code_no_effect_runtime(&fixture.scope)
            .await
            .is_err()
    );
    assert_eq!(
        status(&supervisor.ledger, &fixture.scope).await,
        (false, Some("cleanup_unconfirmed".into()))
    );
    assert!(!state.lock().unwrap().present);
    // Replacement process observes the original missing CID but must finish its exact cleanup.
    let supervisor =
        DockerSupervisor::new(supervisor.ledger, Runtime(state.clone()), owner.clone(), 1).unwrap();
    supervisor
        .cleanup_code_no_effect_runtime(&fixture.scope)
        .await
        .unwrap();
    assert_eq!(
        status(&supervisor.ledger, &fixture.scope).await,
        (true, None)
    );
    assert_eq!(
        state.lock().unwrap().calls,
        ["terminate", "terminate", "cleanup", "terminate", "cleanup"]
    );
    assert!(
        supervisor
            .ledger
            .claim(&fixture.scope, owner.clone(), 30)
            .await
            .unwrap()
            .is_none()
    );
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn cleanup_lease_expiry_fences_prior_writer_and_never_becomes_execution_authority() {
    let ledger = ledger().await;
    let job = sealed(&ledger).await;
    let (fixture, execution_lease, owner) = (&job.fixture, &job.lease, &job.owner);
    let first = ledger
        .claim_code_no_effect_cleanup(&fixture.scope, owner, 30)
        .await
        .unwrap()
        .unwrap();
    assert!(
        ledger
            .claim_code_no_effect_cleanup(&fixture.scope, owner, 30)
            .await
            .unwrap()
            .is_none()
    );
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
        .bind(&fixture.scope.tenant).bind(fixture.scope.project).bind(fixture.scope.key.as_slice()).execute(&ledger.pool).await.unwrap();
    let second = ledger
        .claim_code_no_effect_cleanup(&fixture.scope, owner, 30)
        .await
        .unwrap()
        .unwrap();
    assert!(ledger.check_code_no_effect_cleanup(&first).await.is_err());
    assert!(
        ledger
            .record_code_no_effect_cleanup(&first, None)
            .await
            .is_err()
    );
    assert!(ledger.mark_dispatched(execution_lease).await.is_err());
    ledger.check_code_no_effect_cleanup(&second).await.unwrap();
    ledger
        .record_code_no_effect_cleanup(&second, None)
        .await
        .unwrap();
    assert_eq!(status(&ledger, &fixture.scope).await, (true, None));
}
