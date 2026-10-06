//! Required-mode disposable PG18 fixture; authored source, never runtime acceptance.
use super::*;
use crate::state::postgres_session_tests::IsolatedPostgres;
use sqlx::{
    ConnectOptions as _,
    postgres::{PgConnectOptions, PgPoolOptions},
};
use std::{str::FromStr as _, time::Duration};

async fn database(install_workspace: bool) -> IsolatedPostgres {
    assert_eq!(
        std::env::var("ELITEA_TEST_WORKSPACE_POSTGRES_REQUIRED").as_deref(),
        Ok("1"),
        "required workspace PostgreSQL mode must be explicit"
    );
    let url = std::env::var("ELITEA_TEST_DATABASE_URL")
        .expect("a disposable loopback PG18 administrator is required");
    let options = PgConnectOptions::from_str(&url)
        .expect("parse fixture administrator")
        .disable_statement_logging();
    assert!(matches!(
        options.get_host(),
        "localhost" | "127.0.0.1" | "::1"
    ));
    assert_eq!(options.get_database(), Some("postgres"));
    assert_eq!(options.get_username(), "postgres");
    let admin = PgPoolOptions::new()
        .max_connections(1)
        .acquire_timeout(Duration::from_secs(5))
        .connect_with(options)
        .await
        .expect("connect guarded fixture administrator");
    let version: i32 = sqlx::query_scalar("SELECT current_setting('server_version_num')::integer")
        .fetch_one(&admin)
        .await
        .unwrap();
    assert!((180_000..190_000).contains(&version));
    admin.close().await;
    let database = IsolatedPostgres::create(&url).await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&database.pool)
        .await
        .unwrap();
    for source in [
        include_str!("../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"),
        include_str!("../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0006_sandbox_stop_reconciliation.sql"
        ),
        include_str!("../../../elitea-main/migrations/agentstate/0008_sandbox_runtime_binding.sql"),
        include_str!("../../../elitea-main/migrations/agentstate/0010_sandbox_phase_deadlines.sql"),
    ] {
        sqlx::raw_sql(source).execute(&database.pool).await.unwrap();
    }
    if install_workspace {
        sqlx::raw_sql(include_str!(
            "../../../../libs/proto/contracts/code-workspace-runtime-receipt-v1.sql"
        ))
        .execute(&database.pool)
        .await
        .unwrap();
    }
    database
}
fn receipt(scope: &JobScope) -> WorkspaceRuntimeReceipt {
    let runtime = scope.runtime_identity().unwrap();
    WorkspaceRuntimeReceipt {
        revision: 1,
        backend: WorkspaceBackend::Docker,
        project_id: scope.project,
        job_key: runtime.job_key().into(),
        request_digest: runtime.request_digest().into(),
        activation_id: "a".repeat(64),
        original_runtime_id: "original-cid".into(),
        volume_name: format!("elitea-code-repository-{}", runtime.job_key()),
        volume_owner_token: "d".repeat(32),
        volume_creation_token: "2026-10-04T00:00:00Z".into(),
        manifest_sha256: "c".repeat(64),
    }
}
#[tokio::test]
#[ignore = "requires explicit disposable loopback PG18 administrator and required-mode flag"]
async fn postgres_workspace_original_receipt_survives_terminal_container_removal_and_fences_stale_writer()
 {
    let db = database(true).await;
    let ledger = JobLedger::new(db.pool.clone());
    let scope = JobScope::new("workspace-fixture".into(), 7, [1; 32], [2; 32]).unwrap();
    ledger.reserve(&scope).await.unwrap();
    let first = ledger
        .claim(&scope, "first-owner".into(), 60)
        .await
        .unwrap()
        .unwrap();
    ledger.bind_runtime(&first, "original-cid").await.unwrap();
    let original = receipt(&scope);
    ledger
        .bind_workspace_runtime(&first, &original)
        .await
        .unwrap();
    ledger
        .bind_workspace_runtime(&first, &original)
        .await
        .unwrap();
    let mut changed = original.clone();
    changed.volume_owner_token = "e".repeat(32);
    assert!(
        ledger
            .bind_workspace_runtime(&first, &changed)
            .await
            .is_err()
    );
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second'",
    )
    .execute(&db.pool)
    .await
    .unwrap();
    let replacement = ledger
        .claim(&scope, "replacement-owner".into(), 60)
        .await
        .unwrap()
        .unwrap();
    assert!(
        ledger
            .bind_workspace_runtime(&first, &original)
            .await
            .is_err()
    );
    ledger
        .bind_workspace_runtime(&replacement, &original)
        .await
        .unwrap();
    ledger.mark_dispatched(&replacement).await.unwrap();
    ledger
        .finish(
            &replacement,
            Phase::Completed,
            Some("{\"revision\":1,\"status\":\"completed\"}"),
            None,
        )
        .await
        .unwrap();
    // Simulated crash after original CID removal: the next owner needs no container metadata.
    let durable = JobLedger::new(db.pool.clone())
        .terminal_workspace_runtime(&scope)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(
        durable.to_transport().unwrap(),
        original.to_transport().unwrap()
    );
    assert!(
        ledger
            .bind_workspace_runtime(&replacement, &original)
            .await
            .is_err()
    );
}
#[tokio::test]
#[ignore = "requires explicit disposable loopback PG18 administrator and required-mode flag"]
async fn postgres_workspace_legacy_schema_has_no_inferred_cleanup_provenance() {
    let db = database(false).await;
    let ledger = JobLedger::new(db.pool.clone());
    let scope = JobScope::new("workspace-fixture".into(), 7, [1; 32], [2; 32]).unwrap();
    ledger.reserve(&scope).await.unwrap();
    assert!(
        ledger
            .read_workspace_runtime(&scope)
            .await
            .unwrap()
            .is_none()
    );
}
