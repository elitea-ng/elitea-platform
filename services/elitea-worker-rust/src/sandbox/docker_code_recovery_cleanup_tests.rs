//! Actual owner SQL plus an exact-instance runtime double; not deployment proof.
use super::*;
use crate::{protocol::sandbox_grant::code_owner_fixture, sandbox::runtime::CodeJobRuntime};
use adk_sandbox::{
    SandboxError,
    workspace::{Manifest, docker::CodeJobIdentity},
};
use sqlx::{Row as _, postgres::PgPoolOptions};
use std::sync::{Arc, Mutex};

const OWNER: &str = "code-no-effect-cleanup-owner";
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
async fn ledger() -> JobLedger {
    let url = std::env::var("ELITEA_CODE_OWNER_TEST_DATABASE_URL")
        .expect("explicit isolated agentstate database including owner columns required");
    JobLedger::new(
        PgPoolOptions::new()
            .max_connections(2)
            .acquire_timeout(Duration::from_secs(5))
            .connect(&url)
            .await
            .unwrap(),
    )
}
#[allow(
    clippy::format_collect,
    reason = "Keep independent hexadecimal fixture generation separate from production helpers."
)]
async fn sealed(
    ledger: &JobLedger,
) -> (crate::protocol::sandbox_grant::CodeOwnerFixture, JobLease) {
    let fixture = {
        let mut identity = [0_u8; 16];
        ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut identity).unwrap();
        code_owner_fixture(identity.iter().map(|byte| format!("{byte:02x}")).collect())
    };
    ledger.reserve(&fixture.scope).await.unwrap();
    ledger
        .register_code_intent(&fixture.scope, &fixture.intent)
        .await
        .unwrap();
    let lease = ledger
        .claim(&fixture.scope, OWNER.to_owned(), 30)
        .await
        .unwrap()
        .unwrap();
    ledger.bind_runtime(&lease, INSTANCE).await.unwrap();
    let observed = ledger.observe_code_owner(&fixture.seal).await.unwrap();
    assert!(observed.state == crate::sandbox::ledger::CodeOwnerState::VerifiedNoEffect);
    (fixture, lease)
}
async fn delete(ledger: &JobLedger, scope: &JobScope) {
    sqlx::query("DELETE FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4")
        .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).execute(&ledger.pool).await.unwrap();
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
    let (fixture, old_lease) = sealed(&ledger).await;
    let proof = ledger
        .observe_code_owner(&fixture.read)
        .await
        .unwrap()
        .receipt
        .unwrap()
        .canonical_bytes()
        .unwrap();
    assert!(ledger.mark_dispatched(&old_lease).await.is_err());
    assert!(
        ledger
            .pending_code_no_effect_cleanup("other-owner", 32)
            .await
            .unwrap()
            .is_empty()
    );
    let state = Arc::new(Mutex::new(State {
        running: true,
        present: true,
        ..State::default()
    }));
    let supervisor =
        DockerSupervisor::new(ledger, Runtime(state.clone()), OWNER.into(), 1).unwrap();
    supervisor.reconcile_code_no_effect_cleanup().await.unwrap();
    assert_eq!(
        status(&supervisor.ledger, &fixture.scope).await,
        (true, None)
    );
    assert_eq!(state.lock().unwrap().calls, ["terminate", "cleanup"]);
    assert!(
        supervisor
            .ledger
            .claim(&fixture.scope, OWNER.to_owned(), 30)
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
    delete(&supervisor.ledger, &fixture.scope).await;
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn uncertain_termination_then_removed_runtime_cleanup_are_durable_and_idempotent() {
    let ledger = ledger().await;
    let (fixture, _) = sealed(&ledger).await;
    let state = Arc::new(Mutex::new(State {
        running: true,
        present: true,
        termination_failures: 1,
        cleanup_failures: 1,
        ..State::default()
    }));
    let supervisor =
        DockerSupervisor::new(ledger, Runtime(state.clone()), OWNER.into(), 1).unwrap();
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
        DockerSupervisor::new(supervisor.ledger, Runtime(state.clone()), OWNER.into(), 1).unwrap();
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
            .claim(&fixture.scope, OWNER.to_owned(), 30)
            .await
            .unwrap()
            .is_none()
    );
    delete(&supervisor.ledger, &fixture.scope).await;
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn cleanup_lease_expiry_fences_prior_writer_and_never_becomes_execution_authority() {
    let ledger = ledger().await;
    let (fixture, execution_lease) = sealed(&ledger).await;
    let first = ledger
        .claim_code_no_effect_cleanup(&fixture.scope, OWNER, 30)
        .await
        .unwrap()
        .unwrap();
    assert!(
        ledger
            .claim_code_no_effect_cleanup(&fixture.scope, OWNER, 30)
            .await
            .unwrap()
            .is_none()
    );
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
        .bind(&fixture.scope.tenant).bind(fixture.scope.project).bind(fixture.scope.key.as_slice()).execute(&ledger.pool).await.unwrap();
    let second = ledger
        .claim_code_no_effect_cleanup(&fixture.scope, OWNER, 30)
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
    assert!(ledger.mark_dispatched(&execution_lease).await.is_err());
    ledger.check_code_no_effect_cleanup(&second).await.unwrap();
    ledger
        .record_code_no_effect_cleanup(&second, None)
        .await
        .unwrap();
    assert_eq!(status(&ledger, &fixture.scope).await, (true, None));
    delete(&ledger, &fixture.scope).await;
}
