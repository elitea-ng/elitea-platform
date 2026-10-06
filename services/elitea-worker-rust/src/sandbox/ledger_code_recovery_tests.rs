//! These fixtures use the actual owner row/CAS and signed authority verifier.
//! The caller must provision an isolated agentstate DB including the private
//! schema amendment. No fixture provisions a schema or starts a runtime.
use super::*;
use crate::protocol::sandbox_grant::code_owner_fixture;
use sqlx::postgres::PgPoolOptions;
use std::{sync::Arc, time::Duration};

async fn ledger() -> Arc<JobLedger> {
    let url = std::env::var("ELITEA_CODE_OWNER_TEST_DATABASE_URL")
        .expect("explicit isolated agentstate fixture database is required");
    Arc::new(JobLedger::new(
        PgPoolOptions::new()
            .max_connections(4)
            .acquire_timeout(Duration::from_secs(5))
            .connect(&url)
            .await
            .unwrap(),
    ))
}
#[allow(
    clippy::format_collect,
    reason = "Keep independent hexadecimal fixture generation separate from production helpers."
)]
fn fixture() -> crate::protocol::sandbox_grant::CodeOwnerFixture {
    {
        let mut identity = [0_u8; 16];
        ring::rand::SecureRandom::fill(&ring::rand::SystemRandom::new(), &mut identity).unwrap();
        code_owner_fixture(identity.iter().map(|byte| format!("{byte:02x}")).collect())
    }
}
async fn cleanup(ledger: &JobLedger, scope: &JobScope) {
    sqlx::query("DELETE FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3 AND request_digest=$4")
        .bind(&scope.tenant).bind(scope.project).bind(scope.key.as_slice()).bind(scope.digest.as_slice()).execute(&ledger.pool).await.unwrap();
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn code_owner_actual_dispatch_versus_seal_has_one_winner_and_never_replays() {
    let ledger = ledger().await;
    // Exercise both lock orderings through independently scheduled actual SQL.
    for index in 0..16 {
        let f = fixture();
        ledger.reserve(&f.scope).await.unwrap();
        ledger
            .register_code_intent(&f.scope, &f.intent)
            .await
            .unwrap();
        let lease = ledger
            .claim(&f.scope, "race-owner".to_owned(), 30)
            .await
            .unwrap()
            .unwrap();
        ledger
            .bind_runtime(&lease, &format!("fixture-runtime-{index}"))
            .await
            .unwrap();
        let barrier = tokio::sync::Barrier::new(2);
        let (dispatch, seal) = tokio::join!(
            async {
                barrier.wait().await;
                ledger.mark_dispatched(&lease).await
            },
            async {
                barrier.wait().await;
                ledger.observe_code_owner(&f.seal).await.unwrap()
            }
        );
        assert_ne!(
            dispatch.is_ok(),
            (seal.state == CodeOwnerState::VerifiedNoEffect)
        );
        if seal.state == CodeOwnerState::VerifiedNoEffect {
            let exact = seal.receipt.unwrap().canonical_bytes().unwrap();
            assert!(ledger.mark_dispatched(&lease).await.is_err());
            assert!(ledger.renew(&lease, 30).await.is_err());
            assert!(
                ledger
                    .bind_runtime(&lease, "replacement-runtime")
                    .await
                    .is_err()
            );
            assert!(
                ledger
                    .claim(&f.scope, "new-owner".to_owned(), 30)
                    .await
                    .unwrap()
                    .is_none()
            );
            assert!(
                ledger
                    .finish(&lease, Phase::Completed, Some("{}"), None)
                    .await
                    .is_err()
            );
            let replay = ledger.observe_code_owner(&f.read).await.unwrap();
            assert_eq!(replay.receipt.unwrap().canonical_bytes().unwrap(), exact);
            assert_eq!(ledger.reserve(&f.scope).await.unwrap().phase, Phase::Failed);
        } else {
            assert!(seal.state == CodeOwnerState::Pending && seal.receipt.is_none());
            assert!(
                ledger
                    .observe_code_owner(&f.read)
                    .await
                    .unwrap()
                    .receipt
                    .is_none()
            );
            let row=sqlx::query("SELECT dispatched_at FROM elitea_runtime.sandbox_jobs WHERE tenant_id=$1 AND project_id=$2 AND job_key=$3")
                .bind(&f.scope.tenant).bind(f.scope.project).bind(f.scope.key.as_slice()).fetch_one(&ledger.pool).await.unwrap();
            assert!(
                row.try_get::<Option<chrono::DateTime<chrono::Utc>>, _>("dispatched_at")
                    .unwrap()
                    .is_some()
            );
        }
        cleanup(&ledger, &f.scope).await;
    }
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn code_owner_missing_cancelled_and_ambiguous_rows_issue_no_evidence() {
    let ledger = ledger().await;
    let absent = fixture();
    let missing = ledger.observe_code_owner(&absent.seal).await.unwrap();
    assert!(missing.state == CodeOwnerState::Missing && missing.receipt.is_none());
    assert!(matches!(
        ledger.read(&absent.scope).await,
        Err(LedgerError::Missing)
    ));
    for phase in [Phase::Failed, Phase::Uncertain, Phase::Cancelled] {
        let f = fixture();
        ledger.reserve(&f.scope).await.unwrap();
        ledger
            .register_code_intent(&f.scope, &f.intent)
            .await
            .unwrap();
        let lease = ledger
            .claim(&f.scope, "terminal-owner".to_owned(), 30)
            .await
            .unwrap()
            .unwrap();
        ledger.mark_dispatched(&lease).await.unwrap();
        ledger
            .finish(&lease, phase, None, Some("fixture_terminal"))
            .await
            .unwrap();
        let observed = ledger.observe_code_owner(&f.seal).await.unwrap();
        assert!(observed.receipt.is_none());
        assert!(observed.state != CodeOwnerState::VerifiedNoEffect);
        cleanup(&ledger, &f.scope).await;
    }
    let f = fixture();
    ledger.reserve(&f.scope).await.unwrap();
    ledger
        .register_code_intent(&f.scope, &f.intent)
        .await
        .unwrap();
    ledger
        .request_cancellation(&f.scope, "fixture-cancel-owner")
        .await
        .unwrap();
    let cancelled = ledger.observe_code_owner(&f.seal).await.unwrap();
    assert!(cancelled.state == CodeOwnerState::Cancelled && cancelled.receipt.is_none());
    cleanup(&ledger, &f.scope).await;
}

#[tokio::test]
#[ignore = "requires explicit isolated PostgreSQL owner-schema verification window"]
async fn code_owner_completed_exact_receipt_is_immutable_and_unbound_history_is_refused() {
    let ledger = ledger().await;
    let f = fixture();
    ledger.reserve(&f.scope).await.unwrap();
    ledger
        .register_code_intent(&f.scope, &f.intent)
        .await
        .unwrap();
    let lease = ledger
        .claim(&f.scope, "completed-owner".to_owned(), 30)
        .await
        .unwrap()
        .unwrap();
    ledger.mark_dispatched(&lease).await.unwrap();
    let exact = r#"{ "revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":41}","stderr":"private diagnostic" }"#;
    ledger
        .finish(&lease, Phase::Completed, Some(exact), None)
        .await
        .unwrap();
    let first = ledger
        .observe_code_owner(&f.read)
        .await
        .unwrap()
        .receipt
        .unwrap();
    assert_eq!(first.result_bytes().unwrap(), exact.as_bytes());
    assert_eq!(
        ledger
            .observe_code_owner(&f.seal)
            .await
            .unwrap()
            .receipt
            .unwrap(),
        first
    );
    cleanup(&ledger, &f.scope).await;
    let unbound = fixture();
    ledger.reserve(&unbound.scope).await.unwrap();
    let lease = ledger
        .claim(&unbound.scope, "old-owner".to_owned(), 30)
        .await
        .unwrap()
        .unwrap();
    ledger.mark_dispatched(&lease).await.unwrap();
    assert!(
        ledger
            .register_code_intent(&unbound.scope, &unbound.intent)
            .await
            .is_err()
    );
    let observed = ledger.observe_code_owner(&unbound.read).await.unwrap();
    assert!(observed.state == CodeOwnerState::Conflict && observed.receipt.is_none());
    cleanup(&ledger, &unbound.scope).await;
}
