//! A graph checkpoint write behind a stalled writer-row holder fails readably
//! within the shared lock bound instead of waiting forever.

use std::collections::HashMap;
use std::str::FromStr as _;
use std::sync::Arc;
use std::time::{Duration, Instant};

use adk_rust::graph::Checkpoint;
use chrono::Utc;
use sqlx::ConnectOptions as _;
use sqlx::postgres::PgConnectOptions;

use super::{
    APPLICATION_CAPABILITY_ID, CheckpointLimits, CheckpointWriterAuthority,
    PostgresCheckpointError, PostgresCheckpointer,
};
use crate::state::TestStateWriterLease;
use crate::state::postgres_session::WRITER_LOCK_TIMEOUT;
use crate::state::postgres_session_tests::IsolatedPostgres;

const CHECKPOINT_MIGRATION: &str =
    include_str!("../../../elitea-main/migrations/agentstate/0001_agent_graph_checkpoints.sql");

fn authority() -> CheckpointWriterAuthority {
    CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        1,
        APPLICATION_CAPABILITY_ID,
        [0x42; 32],
        "thread-1".to_owned(),
        "execution-1".to_owned(),
        1,
        "claim-1".to_owned(),
        1,
        1,
        1_700_000_001_000_000,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        [0x43; 32],
    )
    .expect("valid checkpoint writer")
}

fn checkpoint(checkpoint_id: &str) -> Checkpoint {
    Checkpoint {
        thread_id: "thread-1".to_owned(),
        checkpoint_id: checkpoint_id.to_owned(),
        state: HashMap::new(),
        step: 1,
        pending_nodes: Vec::new(),
        metadata: HashMap::new(),
        created_at: Utc::now(),
        cleared_interrupt: None,
        attempts: HashMap::new(),
        child_ledger: HashMap::new(),
    }
}

#[tokio::test]
async fn checkpoint_write_behind_a_stalled_writer_fails_typed_within_the_lock_bound() {
    let Ok(url) = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("skipping checkpoint lock-bound test: set ELITEA_TEST_DATABASE_URL");
        return;
    };
    let database = IsolatedPostgres::create(&url).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&database.pool)
        .await
        .expect("apply checkpoint migration");
    // Its own pool with a generous acquire bound: under a loaded suite a pool
    // timeout must not stand in for the lock timeout this test is about.
    let pool = crate::test_pg::pool_options()
        .max_connections(2)
        .acquire_timeout(Duration::from_mins(1))
        .connect_with(
            PgConnectOptions::from_str(&url)
                .expect("parse test URL")
                .disable_statement_logging()
                .database(&database.database_name),
        )
        .await
        .expect("connect checkpoint pool");
    let checkpointer = PostgresCheckpointer::activate(
        pool.clone(),
        authority(),
        CheckpointLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("activate checkpoint writer");

    let mut holder = database.pool.begin().await.expect("begin holder");
    sqlx::query(
        "SELECT 1 FROM elitea_runtime.agent_graph_checkpoint_writers WHERE thread_id = 'thread-1' FOR UPDATE",
    )
    .fetch_one(&mut *holder)
    .await
    .expect("hold the checkpoint writer row");
    let started = Instant::now();
    let error = tokio::time::timeout(
        WRITER_LOCK_TIMEOUT + Duration::from_secs(10),
        checkpointer.save_checkpoint(&checkpoint("blocked")),
    )
    .await
    .expect("the checkpoint write must not wait forever")
    .expect_err("a stalled writer lock fails the write");
    let waited = started.elapsed();
    let PostgresCheckpointError::StorageUnavailable { source } = &error else {
        panic!(
            "a lock wait is retryable storage unavailability: {}",
            error.code()
        );
    };
    assert!(
        matches!(source, sqlx::Error::Database(database) if database.code().as_deref() == Some("55P03")),
        "the failure is PostgreSQL's lock timeout, not another storage error"
    );
    assert!(waited >= WRITER_LOCK_TIMEOUT, "{waited:?}");
    holder.rollback().await.expect("release holder");

    checkpointer
        .save_checkpoint(&checkpoint("after-release"))
        .await
        .expect("the write succeeds once the holder is gone");
}
