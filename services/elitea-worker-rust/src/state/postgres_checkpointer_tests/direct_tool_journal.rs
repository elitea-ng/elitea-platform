//! Real-`PostgreSQL` crash-window proofs for effectful pipeline direct tool nodes.
//! The node runs behind the fenced Started journal that `PostgresCheckpointer` provides.
use super::*;
use crate::agents::graph::node_recovery::NodeRecoveryPolicy;
use crate::agents::graph::node_recovery_runtime::NodeRecoveryFactory;
use crate::agents::graph::{
    Gate, direct_tool_state, journaled_node, node_activation, pause_data, recovery_card,
    with_decision,
};
use adk_rust::graph::END;
use adk_rust::graph::{Node, NodeContext};
use serde_json::Value;
use tokio::task::JoinHandle;

const JOURNAL_KEY: &str = "elitea.pipeline.node-attempt-journal.v1";
const STEP: usize = 2;
const WAIT: Duration = Duration::from_secs(10);

struct Journal {
    step: i64,
    has_result: bool,
    claim: String,
}

async fn database() -> IsolatedPostgres {
    let url = env::var(TEST_DATABASE_URL).expect("ELITEA_TEST_DATABASE_URL is required");
    let database = IsolatedPostgres::create(&url).await;
    install_test_schema(&database.pool).await;
    database
}

/// A fresh pool is a replaced process: none of the previous connections or state survive.
async fn fresh_pool(database: &IsolatedPostgres) -> PgPool {
    PgPoolOptions::new()
        .max_connections(4)
        .acquire_timeout(Duration::from_secs(2))
        .connect_with(
            database
                .admin_options
                .clone()
                .database(&database.database_name),
        )
        .await
        .expect("connect replacement process pool")
}

async fn checkpointer(
    pool: PgPool,
    claim_attempt: u64,
    lease: &Arc<TestStateWriterLease>,
) -> Arc<PostgresCheckpointer> {
    Arc::new(
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
            lease.clone(),
        )
        .await
        .expect("activate direct-tool journal writer"),
    )
}

fn context(state: State) -> NodeContext {
    NodeContext::new(state, ExecutionConfig::new("thread-1"), STEP)
}

/// Every node-attempt journal row, oldest first, as stored in `PostgreSQL`.
async fn journal(pool: &PgPool) -> Vec<Journal> {
    sqlx::query_as::<_, (i64, bool, String)>(
        r"
SELECT step,
       coalesce(jsonb_typeof(metadata::jsonb -> $1 -> 'completed_updates'), 'null') <> 'null',
       writer_claim_id
FROM elitea_runtime.agent_graph_checkpoints
WHERE thread_id LIKE 'n1:%'
ORDER BY thread_id, save_ordinal
        ",
    )
    .bind(JOURNAL_KEY)
    .fetch_all(pool)
    .await
    .expect("read node-attempt journal rows")
    .into_iter()
    .map(|(step, has_result, claim)| Journal {
        step,
        has_result,
        claim,
    })
    .collect()
}

async fn journal_writers(pool: &PgPool) -> i64 {
    sqlx::query_scalar(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoint_writers WHERE thread_id LIKE 'n1:%'",
    )
    .fetch_one(pool)
    .await
    .expect("count node-attempt journal writers")
}

fn spawn_execute(node: &Arc<dyn Node>, state: State) -> JoinHandle<()> {
    let node = Arc::clone(node);
    tokio::spawn(async move {
        let _ = node.execute(&context(state)).await;
    })
}

async fn entered(gate: &Gate) {
    tokio::time::timeout(WAIT, gate.entered.notified())
        .await
        .expect("the effectful tool was reached");
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn committed_effect_result_survives_process_replacement() {
    let database = database().await;
    let first_pool = fresh_pool(&database).await;
    let first = checkpointer(
        first_pool.clone(),
        1,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let (node, calls) = journaled_node(first, None, false);
    let completed = node.execute(&context(direct_tool_state())).await.unwrap();
    assert_eq!(completed.updates["report"], json!({"created": true}));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let rows = journal(&database.pool).await;
    assert_eq!(
        rows.iter()
            .map(|row| (row.step, row.has_result))
            .collect::<Vec<_>>(),
        [(1, false), (2, true)],
        "Started, then the committed result"
    );

    // The process dies: its connections are gone and a new instance takes the same claim.
    drop(node);
    first_pool.close().await;
    let replacement = checkpointer(
        fresh_pool(&database).await,
        1,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let (node, replacement_calls) = journaled_node(replacement, None, false);
    let replayed = node.execute(&context(direct_tool_state())).await.unwrap();
    assert_eq!(replayed.updates, completed.updates);
    assert_eq!(replacement_calls.load(Ordering::SeqCst), 0);
    assert_eq!(journal(&database.pool).await.len(), 2);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn crash_after_started_before_result_never_repeats_the_effect() {
    let database = database().await;
    let first_pool = fresh_pool(&database).await;
    let first = checkpointer(
        first_pool.clone(),
        1,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let gate = Arc::new(Gate {
        entered: tokio::sync::Notify::new(),
        release: tokio::sync::Notify::new(),
    });
    let (node, calls) = journaled_node(first, Some(Arc::clone(&gate)), false);
    // The process dies while the effect is in flight: the future is dropped mid-call.
    let running = spawn_execute(&node, direct_tool_state());
    entered(&gate).await;
    running.abort();
    assert!(
        running
            .await
            .expect_err("the execution was killed")
            .is_cancelled()
    );
    drop(node);
    first_pool.close().await;
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    let rows = journal(&database.pool).await;
    assert_eq!(
        rows.iter()
            .map(|row| (row.step, row.has_result))
            .collect::<Vec<_>>(),
        [(1, false)],
        "a durable Started row with no result"
    );

    let replacement = checkpointer(
        fresh_pool(&database).await,
        1,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let (node, replacement_calls) = journaled_node(replacement, None, false);
    for _ in 0..2 {
        let output = node.execute(&context(direct_tool_state())).await.unwrap();
        assert!(recovery_card(&output));
        assert!(output.updates.is_empty());
    }
    assert_eq!(replacement_calls.load(Ordering::SeqCst), 0);
    // The unknown effect is recorded once and is never completed by the replacement.
    let rows = journal(&database.pool).await;
    assert_eq!(rows.len(), 2);
    assert!(rows.iter().all(|row| !row.has_result));
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn second_claim_takeover_fences_the_old_writer_and_keeps_the_started_effect() {
    let database = database().await;
    let lease_a = Arc::new(TestStateWriterLease::current());
    let claim_a = checkpointer(database.pool.clone(), 1, &lease_a).await;
    let gate = Arc::new(Gate {
        entered: tokio::sync::Notify::new(),
        release: tokio::sync::Notify::new(),
    });
    let (node_a, calls_a) = journaled_node(claim_a.clone(), Some(Arc::clone(&gate)), false);
    let running = spawn_execute(&node_a, direct_tool_state());
    entered(&gate).await;
    assert_eq!(calls_a.load(Ordering::SeqCst), 1);
    assert_eq!(journal(&database.pool).await.len(), 1);

    // Claim B is newer and takes the execution over while A is inside the tool.
    let claim_b = checkpointer(
        database.pool.clone(),
        2,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let (node_b, calls_b) = journaled_node(claim_b, None, false);
    let output = node_b.execute(&context(direct_tool_state())).await.unwrap();
    assert!(recovery_card(&output));
    assert_eq!(calls_b.load(Ordering::SeqCst), 0);

    // A's lease is still current in its process, yet PostgreSQL no longer admits it.
    let activation = node_activation(&context(direct_tool_state()));
    let Err(fenced) = claim_a
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
    else {
        panic!("a superseded claim opened the node journal");
    };
    assert!(
        fenced.to_string().contains("writer_not_current"),
        "{fenced}"
    );
    let again = tokio::time::timeout(WAIT, node_a.execute(&context(direct_tool_state())))
        .await
        .expect("the superseded claim stopped before dispatching");
    assert!(
        again.is_err(),
        "the superseded claim re-dispatched the tool"
    );
    assert_eq!(calls_a.load(Ordering::SeqCst), 1);

    // A's effect finishes, but its result cannot be appended behind B's fence.
    gate.release.notify_one();
    tokio::time::timeout(WAIT, running)
        .await
        .expect("the fenced execution finished")
        .unwrap();
    let rows = journal(&database.pool).await;
    assert!(
        rows.iter().all(|row| !row.has_result),
        "A recorded a result"
    );
    assert_eq!(
        rows.iter()
            .map(|row| row.claim.as_str())
            .collect::<Vec<_>>(),
        ["claim-1", "claim-2"],
        "A's Started, then B's unknown-effect record"
    );

    // Once A's lease is revoked it cannot even open the journal.
    lease_a.revoke();
    let Err(revoked) = claim_a
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
    else {
        panic!("a revoked writer opened the node journal");
    };
    assert!(
        revoked
            .to_string()
            .contains("pipeline.node_recovery.writer_not_current"),
        "{revoked}"
    );
    assert!(node_a.execute(&context(direct_tool_state())).await.is_err());
    // B re-entering still sees the durable attempt and never repeats the effect.
    let output = node_b.execute(&context(direct_tool_state())).await.unwrap();
    assert!(recovery_card(&output));
    assert_eq!(calls_a.load(Ordering::SeqCst), 1);
    assert_eq!(calls_b.load(Ordering::SeqCst), 0);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn superseded_claim_cannot_start_an_effect_in_an_unopened_activation() {
    let database = database().await;
    let claim_a = checkpointer(
        database.pool.clone(),
        1,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let (node_a, calls_a) = journaled_node(claim_a.clone(), None, false);
    // Claim B takes the run over before A reaches this node. A's in-process lease is
    // still current, so only the root writer row in PostgreSQL can refuse it.
    let claim_b = checkpointer(
        database.pool.clone(),
        2,
        &Arc::new(TestStateWriterLease::current()),
    )
    .await;

    let activation = node_activation(&context(direct_tool_state()));
    let Err(fenced) = claim_a
        .open(&activation, &NodeRecoveryPolicy::default())
        .await
    else {
        panic!("a superseded claim opened a new node journal");
    };
    assert!(
        fenced.to_string().contains("writer_not_current"),
        "{fenced}"
    );
    assert!(node_a.execute(&context(direct_tool_state())).await.is_err());
    assert_eq!(calls_a.load(Ordering::SeqCst), 0);
    assert_eq!(journal_writers(&database.pool).await, 0);

    // The current owner opens the same activation and runs the effect exactly once.
    let (node_b, calls_b) = journaled_node(claim_b, None, false);
    let output = node_b.execute(&context(direct_tool_state())).await.unwrap();
    assert!(!recovery_card(&output));
    assert_eq!(calls_b.load(Ordering::SeqCst), 1);
    assert_eq!(calls_a.load(Ordering::SeqCst), 0);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn pause_and_block_leave_no_postgres_journal_rows() {
    let database = database().await;
    for (action, comment) in [("reject", None), ("block_with_comment", Some("no"))] {
        let lease = Arc::new(TestStateWriterLease::current());
        let (node, calls) = journaled_node(
            checkpointer(database.pool.clone(), 1, &lease).await,
            None,
            true,
        );
        let data = pause_data(node.execute(&context(direct_tool_state())).await.unwrap());
        assert_eq!(data["guardrail_type"], "sensitive_tool");
        assert_eq!(calls.load(Ordering::SeqCst), 0);
        assert!(journal(&database.pool).await.is_empty());
        assert_eq!(journal_writers(&database.pool).await, 0);

        let blocked = with_decision(direct_tool_state(), &data, action, comment);
        let output = node.execute(&context(blocked)).await.unwrap();
        assert_eq!(output.goto, Some(vec![END.to_owned()]));
        assert!(!output.updates.contains_key("report"));
        assert_eq!(calls.load(Ordering::SeqCst), 0);
        assert!(journal(&database.pool).await.is_empty());
        assert_eq!(journal_writers(&database.pool).await, 0);
    }

    // The same pause, once approved, is the first thing to reach the journal.
    let lease = Arc::new(TestStateWriterLease::current());
    let (node, calls) = journaled_node(
        checkpointer(database.pool.clone(), 1, &lease).await,
        None,
        true,
    );
    let data = pause_data(node.execute(&context(direct_tool_state())).await.unwrap());
    let approved = with_decision(direct_tool_state(), &data, "approve", None);
    let output = node.execute(&context(approved)).await.unwrap();
    assert_eq!(output.updates["report"], json!({"created": true}));
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(journal(&database.pool).await.len(), 2);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn replaying_a_completed_activation_never_repeats_the_effect() {
    let database = database().await;
    let lease = Arc::new(TestStateWriterLease::current());
    let (node, calls) = journaled_node(
        checkpointer(database.pool.clone(), 1, &lease).await,
        None,
        false,
    );
    let first = node.execute(&context(direct_tool_state())).await.unwrap();
    for _ in 0..3 {
        let replay = node.execute(&context(direct_tool_state())).await.unwrap();
        assert_eq!(
            serde_json::to_value(&replay.updates).unwrap(),
            serde_json::to_value(&first.updates).unwrap()
        );
        assert!(replay.interrupt.is_none() && replay.goto.is_none());
    }
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        journal(&database.pool).await.len(),
        2,
        "replay appends nothing"
    );
    // A different activation (changed input) is a new effect with its own journal.
    let mut other = State::new();
    other.insert("input".to_owned(), Value::from(8));
    node.execute(&context(other)).await.unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 2);
}
