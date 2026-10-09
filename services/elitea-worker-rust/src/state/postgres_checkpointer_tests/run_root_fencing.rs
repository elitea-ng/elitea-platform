//! Every thread a run opens below its root is fenced by the run's root writer, not
//! by its own parent: a claim superseded at the root cannot open new threads even
//! while its in-process lease and its older child threads are still current.
use super::*;
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::{NodeAttemptActivation, NodeRecoveryFactory};

async fn database() -> IsolatedPostgres {
    let url = env::var(TEST_DATABASE_URL).expect("ELITEA_TEST_DATABASE_URL is required");
    let database = IsolatedPostgres::create(&url).await;
    install_test_schema(&database.pool).await;
    database
}

async fn root(pool: PgPool, claim_attempt: u64) -> PostgresCheckpointer {
    PostgresCheckpointer::activate(
        pool,
        writer(
            "execution-1",
            &format!("claim-{claim_attempt}"),
            claim_attempt,
            claim_attempt,
            [0x41; 32],
            [0x61; 32],
        ),
        CheckpointLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("activate the run root writer")
}

async fn writers(pool: &PgPool, thread_pattern: &str) -> i64 {
    sqlx::query_scalar(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoint_writers WHERE thread_id LIKE $1",
    )
    .bind(thread_pattern)
    .fetch_one(pool)
    .await
    .expect("count checkpoint writers")
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn superseded_claim_cannot_activate_application_child_threads() {
    let database = database().await;
    let claim_a = root(database.pool.clone(), 1).await;
    let claim_b = root(database.pool.clone(), 2).await;

    let Err(error) = claim_a
        .with_application_children(["delegate"].into_iter())
        .await
    else {
        panic!("a superseded claim activated an application child thread");
    };
    assert!(
        matches!(error, PostgresCheckpointError::WriterNotCurrent),
        "{error:?}"
    );
    assert_eq!(writers(&database.pool, "thread-1/%").await, 0);

    claim_b
        .with_application_children(["delegate"].into_iter())
        .await
        .expect("the current owner activates its child thread");
    assert_eq!(writers(&database.pool, "thread-1/%").await, 1);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn child_thread_of_a_superseded_run_cannot_open_new_threads() {
    let database = database().await;
    let family = root(database.pool.clone(), 2)
        .await
        .with_application_children(["delegate"].into_iter())
        .await
        .expect("activate the child thread before the takeover");
    let child = family
        .for_thread("thread-1/delegate")
        .expect("admitted child thread");

    // A newer claim takes over the run root only. The old child thread is still
    // owned by the old claim, so only the run root can refuse what it opens next.
    let _claim_c = root(database.pool.clone(), 3).await;

    let activation = NodeAttemptActivation {
        root_thread_id: "thread-1/delegate".to_owned(),
        node_id: "lookup".to_owned(),
        step: 2,
        node_digest: [0x07; 32],
        input_digest: [0x09; 32],
    };
    let Err(error) = child
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
    else {
        panic!("a child thread of a superseded run opened a node journal");
    };
    assert!(error.to_string().contains("writer_not_current"), "{error}");
    assert_eq!(writers(&database.pool, "n1:%").await, 0);
}
