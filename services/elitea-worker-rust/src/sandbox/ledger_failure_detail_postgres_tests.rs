//! The failed run's stderr tail on the real ledger (#2223). Required-mode
//! disposable PG18 fixture, like the workspace receipt tests.
use super::*;
use crate::state::postgres_session_tests::IsolatedPostgres;
use std::time::Duration;

async fn database(with_column: bool) -> IsolatedPostgres {
    assert_eq!(
        std::env::var("ELITEA_TEST_WORKSPACE_POSTGRES_REQUIRED").as_deref(),
        Ok("1"),
        "required PostgreSQL mode must be explicit"
    );
    let url = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL")
        .expect("a disposable loopback PG18 administrator is required");
    let options = crate::test_pg::connect_options(&url)
        .expect("parse fixture administrator")
        .disable_statement_logging();
    assert!(matches!(
        options.get_host(),
        "localhost" | "127.0.0.1" | "::1"
    ));
    let admin = crate::test_pg::pool_options()
        .max_connections(1)
        .acquire_timeout(Duration::from_secs(5))
        .connect_with(options)
        .await
        .expect("connect guarded fixture administrator");
    admin.close().await;
    let database = IsolatedPostgres::create(&url).await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&database.pool)
        .await
        .unwrap();
    let mut migrations = vec![
        include_str!("../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"),
        include_str!("../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"),
        include_str!(
            "../../../elitea-main/migrations/agentstate/0006_sandbox_stop_reconciliation.sql"
        ),
        include_str!("../../../elitea-main/migrations/agentstate/0008_sandbox_runtime_binding.sql"),
        include_str!("../../../elitea-main/migrations/agentstate/0010_sandbox_phase_deadlines.sql"),
    ];
    if with_column {
        migrations.push(include_str!(
            "../../../elitea-main/migrations/agentstate/0015_sandbox_failure_detail.sql"
        ));
    }
    for source in migrations {
        sqlx::raw_sql(source).execute(&database.pool).await.unwrap();
    }
    database
}

async fn dispatched(ledger: &JobLedger, tag: u8) -> (JobScope, JobLease) {
    let scope = JobScope::new("detail-fixture".into(), 7, [tag; 32], [tag + 1; 32]).unwrap();
    ledger.reserve(&scope).await.unwrap();
    let lease = ledger
        .claim(&scope, "owner".into(), 60)
        .await
        .unwrap()
        .unwrap();
    ledger.mark_dispatched(&lease).await.unwrap();
    (scope, lease)
}

#[tokio::test]
#[ignore = "requires explicit disposable loopback PG18 administrator and required-mode flag"]
async fn postgres_failed_run_keeps_the_stderr_tail_and_nothing_else_does() {
    let db = database(true).await;
    let ledger = JobLedger::new(db.pool.clone());

    let (scope, lease) = dispatched(&ledger, 1).await;
    ledger
        .finish_with_detail(
            &lease,
            Phase::Failed,
            None,
            Some("sandbox.code_failed"),
            Some("error[E0308]: mismatched types"),
        )
        .await
        .unwrap();
    let record = ledger.read(&scope).await.unwrap();
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(record.failure_code.as_deref(), Some("sandbox.code_failed"));
    assert_eq!(
        record.failure_detail.as_deref(),
        Some("error[E0308]: mismatched types")
    );
    assert!(record.result_json.is_none());

    // A failure with no detail, and a completed run that is handed one: the
    // detail is kept only on a failed row.
    let (plain, lease) = dispatched(&ledger, 3).await;
    ledger
        .finish(
            &lease,
            Phase::Failed,
            None,
            Some("sandbox.deadline_exceeded"),
        )
        .await
        .unwrap();
    assert_eq!(ledger.read(&plain).await.unwrap().failure_detail, None);
    let (done, lease) = dispatched(&ledger, 5).await;
    ledger
        .finish_with_detail(
            &lease,
            Phase::Completed,
            Some("{\"revision\":1,\"status\":\"completed\"}"),
            None,
            Some("ignored"),
        )
        .await
        .unwrap();
    assert_eq!(ledger.read(&done).await.unwrap().failure_detail, None);

    // An oversized or NUL-bearing detail is dropped, never an error: the
    // failure code is what the receipt rests on.
    for (tag, bad) in [(7, "x".repeat(9_000)), (9, "a\0b".to_owned())] {
        let (scope, lease) = dispatched(&ledger, tag).await;
        ledger
            .finish_with_detail(
                &lease,
                Phase::Failed,
                None,
                Some("sandbox.code_failed"),
                Some(&bad),
            )
            .await
            .unwrap();
        let record = ledger.read(&scope).await.unwrap();
        assert_eq!(record.phase, Phase::Failed);
        assert_eq!(record.failure_detail, None);
    }

    // The table itself refuses a detail on a row that did not fail.
    assert!(
        sqlx::query(
            "UPDATE elitea_runtime.sandbox_jobs SET failure_detail='x' WHERE phase='completed'"
        )
        .execute(&db.pool)
        .await
        .is_err()
    );
}

#[tokio::test]
#[ignore = "requires explicit disposable loopback PG18 administrator and required-mode flag"]
async fn postgres_schema_without_the_column_still_finishes_and_reads_failures() {
    let db = database(false).await;
    let ledger = JobLedger::new(db.pool.clone());
    let (scope, lease) = dispatched(&ledger, 1).await;
    ledger
        .finish_with_detail(
            &lease,
            Phase::Failed,
            None,
            Some("sandbox.code_failed"),
            Some("error[E0308]"),
        )
        .await
        .unwrap();
    let record = ledger.read(&scope).await.unwrap();
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(record.failure_code.as_deref(), Some("sandbox.code_failed"));
    assert_eq!(record.failure_detail, None);
}
