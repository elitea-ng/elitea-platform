use std::collections::HashMap;
use std::env;
use std::str::FromStr;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{
    Checkpoint, Checkpointer, END, ExecutionConfig, NodeOutput, START, State, StateGraph,
};
use chrono::{DateTime, Utc};
use serde_json::json;
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::{ConnectOptions, PgPool};

use super::TestStateWriterLease;
use super::postgres_checkpointer::{
    APPLICATION_CAPABILITY_ID, CheckpointLimits, CheckpointWriterAuthority,
    PostgresCheckpointError, PostgresCheckpointer,
};
use crate::agents::graph::{
    ParallelActivation, ParallelChildCheckpointerFactory, ParallelNodeDefinition,
};

const TEST_DATABASE_URL: &str = "ELITEA_TEST_DATABASE_URL";
const CHECKPOINT_MIGRATION: &str =
    include_str!("../../../elitea-main/migrations/agentstate/0001_agent_graph_checkpoints.sql");

struct IsolatedPostgres {
    pool: PgPool,
    admin_options: PgConnectOptions,
    database_name: String,
}

impl IsolatedPostgres {
    async fn create(database_url: &str) -> Self {
        let admin_options = PgConnectOptions::from_str(database_url)
            .expect("parse PostgreSQL component-test URL")
            .disable_statement_logging();
        let admin_pool = PgPoolOptions::new()
            .max_connections(2)
            .acquire_timeout(Duration::from_secs(5))
            .connect_with(admin_options.clone())
            .await
            .expect("connect PostgreSQL component-test administrator");
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("system time")
            .as_nanos();
        let database_name = format!("elitea_rust_checkpoint_{}_{}", std::process::id(), unique);
        sqlx::query(&format!("CREATE DATABASE {database_name}"))
            .execute(&admin_pool)
            .await
            .expect("create isolated checkpoint database");
        admin_pool.close().await;

        let pool = PgPoolOptions::new()
            .min_connections(1)
            .max_connections(4)
            .acquire_timeout(Duration::from_secs(2))
            .idle_timeout(Duration::from_secs(30))
            .connect_with(admin_options.clone().database(&database_name))
            .await
            .expect("connect isolated checkpoint database");
        Self {
            pool,
            admin_options,
            database_name,
        }
    }
}

impl Drop for IsolatedPostgres {
    fn drop(&mut self) {
        let admin_options = self.admin_options.clone();
        let database_name = self.database_name.clone();
        std::thread::spawn(move || {
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("build PostgreSQL cleanup runtime")
                .block_on(async move {
                    let _ = tokio::time::timeout(Duration::from_secs(10), async move {
                        if let Ok(admin_pool) = PgPoolOptions::new()
                            .max_connections(1)
                            .acquire_timeout(Duration::from_secs(5))
                            .connect_with(admin_options)
                            .await
                        {
                            let _ =
                                sqlx::query(&format!("DROP DATABASE {database_name} WITH (FORCE)"))
                                    .execute(&admin_pool)
                                    .await;
                            admin_pool.close().await;
                        }
                    })
                    .await;
                });
        })
        .join()
        .expect("join PostgreSQL cleanup thread");
    }
}

async fn install_test_schema(pool: &PgPool) {
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(pool)
        .await
        .expect("apply checkpoint migration");
}

async fn wait_for_blocked_query(pool: &PgPool, fragment: &str) {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            let blocked = sqlx::query_scalar::<_, bool>(
                r"
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE pid <> pg_backend_pid()
      AND state = 'active'
      AND wait_event_type = 'Lock'
      AND position($1 in query) > 0
)
                ",
            )
            .bind(fragment)
            .fetch_one(pool)
            .await
            .expect("observe blocked PostgreSQL component query");
            if blocked {
                break;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("query reached the expected PostgreSQL lock barrier");
}

fn writer(
    execution_id: &str,
    claim_id: &str,
    claim_attempt: u64,
    lease_epoch: u64,
    definition_digest: [u8; 32],
    fence_token: [u8; 32],
) -> CheckpointWriterAuthority {
    writer_at(
        execution_id,
        claim_id,
        claim_attempt,
        lease_epoch,
        definition_digest,
        fence_token,
        1_700_000_000_000_000 + i64::try_from(claim_attempt).expect("claim attempt") * 1_000_000,
    )
}

fn writer_at(
    execution_id: &str,
    claim_id: &str,
    claim_attempt: u64,
    lease_epoch: u64,
    definition_digest: [u8; 32],
    fence_token: [u8; 32],
    claim_started_at_unix_micros: i64,
) -> CheckpointWriterAuthority {
    CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        1,
        APPLICATION_CAPABILITY_ID,
        definition_digest,
        "thread-1".to_owned(),
        execution_id.to_owned(),
        1,
        claim_id.to_owned(),
        claim_attempt,
        lease_epoch,
        claim_started_at_unix_micros,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        fence_token,
    )
    .expect("valid checkpoint writer")
}

fn checkpoint(checkpoint_id: &str, created_at: &str, step: usize) -> Checkpoint {
    Checkpoint {
        thread_id: "thread-1".to_owned(),
        checkpoint_id: checkpoint_id.to_owned(),
        state: HashMap::from([
            (
                "messages".to_owned(),
                json!([{"role": "user", "content": "hello"}]),
            ),
            ("counter".to_owned(), json!(step)),
        ]),
        step,
        pending_nodes: vec!["review".to_owned(), "publish".to_owned()],
        metadata: HashMap::from([("route".to_owned(), json!("safe"))]),
        created_at: DateTime::parse_from_rfc3339(created_at)
            .expect("fixture timestamp")
            .with_timezone(&Utc),
        cleared_interrupt: Some("approval".to_owned()),
        attempts: HashMap::from([("review".to_owned(), 2)]),
        child_ledger: HashMap::from([("child/one".to_owned(), json!({"status": "completed"}))]),
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One component story deliberately preserves lifecycle order.
async fn postgres_checkpointer_round_trips_scopes_fences_prunes_and_releases_pool() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL checkpoint component test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let first_lease = Arc::new(TestStateWriterLease::current());
    let first = Arc::new(
        PostgresCheckpointer::activate(
            database.pool.clone(),
            writer("execution-1", "claim-1", 1, 1, [0x41; 32], [0x61; 32]),
            CheckpointLimits::default(),
            first_lease.clone(),
        )
        .await
        .expect("activate first checkpoint writer"),
    );
    let equal_time = PostgresCheckpointer::activate(
        database.pool.clone(),
        writer_at(
            "execution-equal",
            "claim-equal",
            2,
            2,
            [0x41; 32],
            [0x63; 32],
            1_700_000_001_000_000,
        ),
        CheckpointLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    assert!(matches!(
        equal_time,
        Err(PostgresCheckpointError::WriterNotCurrent)
    ));

    let first_checkpoint = checkpoint("checkpoint-z", "2026-08-13T12:34:56.123456789Z", 1);
    first
        .save(&first_checkpoint)
        .await
        .expect("save checkpoint");
    first
        .save(&first_checkpoint)
        .await
        .expect("exact save replay is idempotent");
    let restored = first
        .load("thread-1")
        .await
        .expect("load latest checkpoint")
        .expect("stored checkpoint");
    assert_eq!(restored.created_at, first_checkpoint.created_at);
    assert_eq!(restored.state, first_checkpoint.state);
    assert_eq!(restored.pending_nodes, first_checkpoint.pending_nodes);
    assert_eq!(restored.metadata, first_checkpoint.metadata);
    assert_eq!(
        restored.cleared_interrupt,
        first_checkpoint.cleared_interrupt
    );
    assert_eq!(restored.attempts, first_checkpoint.attempts);
    assert_eq!(restored.child_ledger, first_checkpoint.child_ledger);
    assert!(first.load("thread-2").await.is_err());

    let second_checkpoint = checkpoint("checkpoint-a", "2026-08-13T12:34:56.123456789Z", 2);
    first
        .save(&second_checkpoint)
        .await
        .expect("save tied checkpoint");
    assert_eq!(
        first
            .load("thread-1")
            .await
            .expect("load deterministic latest")
            .expect("latest checkpoint")
            .checkpoint_id,
        "checkpoint-a"
    );

    let mut numeric_checkpoint =
        checkpoint("checkpoint-number", "2026-08-13T12:34:56.123456790Z", 3);
    numeric_checkpoint.state.insert(
        "huge-number".to_owned(),
        serde_json::from_str("1e400").expect("arbitrary precision JSON number"),
    );
    numeric_checkpoint
        .state
        .insert("representation".to_owned(), json!(1));
    first
        .save(&numeric_checkpoint)
        .await
        .expect("save arbitrary precision checkpoint");
    assert_eq!(
        first
            .load_by_id("checkpoint-number")
            .await
            .expect("load arbitrary precision checkpoint")
            .expect("stored arbitrary precision checkpoint")
            .state,
        numeric_checkpoint.state
    );
    let mut changed_representation = numeric_checkpoint.clone();
    changed_representation
        .state
        .insert("representation".to_owned(), json!(1.0));
    assert!(first.save(&changed_representation).await.is_err());
    assert_eq!(
        first
            .list("thread-1")
            .await
            .expect("list save-ordered checkpoints")
            .into_iter()
            .map(|checkpoint| checkpoint.checkpoint_id)
            .collect::<Vec<_>>(),
        vec!["checkpoint-z", "checkpoint-a", "checkpoint-number"]
    );

    let other_definition = PostgresCheckpointer::activate(
        database.pool.clone(),
        writer("execution-1", "claim-1", 1, 1, [0x42; 32], [0x61; 32]),
        CheckpointLimits::default(),
        first_lease.clone(),
    )
    .await
    .expect("activate isolated graph definition");
    assert!(
        other_definition
            .load_by_id("checkpoint-z")
            .await
            .expect("scoped lookup")
            .is_none()
    );

    let parallel_definition = ParallelNodeDefinition::from_yaml(
        r"
id: gather
type: parallel
branches:
  - id: short
    node: fetch_short
  - id: long
    node: fetch_long
max_concurrency: 2
wait: all
error_policy: fail_after_drain
output: [gathered]
transition: END
        ",
    )
    .expect("valid PostgreSQL parallel fixture");
    let parallel_activation = ParallelActivation {
        root_thread_id: "thread-1".to_owned(),
        node_id: parallel_definition.id().to_owned(),
        step: 4,
        config_digest: parallel_definition.config_digest(),
    };
    let short_branch = &parallel_definition.branches()[0];
    let child_input_digest = [0x42_u8; 32];
    let first_child = first
        .for_branch(&parallel_activation, short_branch, 0, &child_input_digest)
        .await
        .expect("activate first parallel child checkpoint");
    let child_runs = Arc::new(AtomicUsize::new(0));
    let first_runs = Arc::clone(&child_runs);
    let first_child_graph = StateGraph::with_channels(&["result"])
        .add_node_fn("work", move |_| {
            let first_runs = Arc::clone(&first_runs);
            async move {
                first_runs.fetch_add(1, Ordering::SeqCst);
                Ok(NodeOutput::new().with_update("result", json!({"value": "short"})))
            }
        })
        .add_edge(START, "work")
        .add_edge("work", END)
        .compile()
        .expect("compile first parallel child")
        .with_checkpointer_arc(first_child.checkpointer);
    first_child_graph
        .invoke(State::new(), ExecutionConfig::new(&first_child.thread_id))
        .await
        .expect("run first parallel child");

    let recreated_child = first
        .for_branch(&parallel_activation, short_branch, 0, &child_input_digest)
        .await
        .expect("recreate parallel child checkpoint");
    assert_eq!(recreated_child.thread_id, first_child.thread_id);
    let replay_runs = Arc::clone(&child_runs);
    let recreated_child_graph = StateGraph::with_channels(&["result"])
        .add_node_fn("work", move |_| {
            let replay_runs = Arc::clone(&replay_runs);
            async move {
                replay_runs.fetch_add(1, Ordering::SeqCst);
                Ok(NodeOutput::new().with_update("result", json!({"value": "unexpected"})))
            }
        })
        .add_edge(START, "work")
        .add_edge("work", END)
        .compile()
        .expect("compile recreated parallel child")
        .with_checkpointer_arc(recreated_child.checkpointer);
    let replayed = recreated_child_graph
        .invoke(
            State::new(),
            ExecutionConfig::new(&recreated_child.thread_id),
        )
        .await
        .expect("replay terminal parallel child");
    assert_eq!(replayed.get("result"), Some(&json!({"value": "short"})));
    assert_eq!(child_runs.load(Ordering::SeqCst), 1);

    let later_activation = ParallelActivation {
        step: parallel_activation.step + 1,
        ..parallel_activation.clone()
    };
    let later_child = first
        .for_branch(&later_activation, short_branch, 0, &child_input_digest)
        .await
        .expect("activate later loop child checkpoint");
    assert_ne!(later_child.thread_id, first_child.thread_id);
    let changed_input_child = first
        .for_branch(&parallel_activation, short_branch, 0, &[0x43_u8; 32])
        .await
        .expect("activate changed-input child checkpoint");
    assert_ne!(changed_input_child.thread_id, first_child.thread_id);

    sqlx::raw_sql(
        r"
CREATE FUNCTION block_agent_graph_checkpoint_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(640064);
    RETURN NEW;
END
$$;
CREATE TRIGGER block_agent_graph_checkpoint_insert
BEFORE INSERT ON elitea_runtime.agent_graph_checkpoints
FOR EACH ROW EXECUTE FUNCTION block_agent_graph_checkpoint_insert();
        ",
    )
    .execute(&database.pool)
    .await
    .expect("install deterministic checkpoint mutation barrier");
    let mut barrier = database
        .pool
        .begin()
        .await
        .expect("begin checkpoint mutation barrier");
    sqlx::query("SELECT pg_advisory_xact_lock(640064)")
        .execute(&mut *barrier)
        .await
        .expect("hold checkpoint mutation barrier");

    let racing_writer = Arc::clone(&first);
    let mut save_task = tokio::spawn(async move {
        racing_writer
            .save(&checkpoint(
                "checkpoint-race",
                "2026-08-13T12:34:57.123456789Z",
                4,
            ))
            .await
    });
    tokio::select! {
        result = &mut save_task => {
            panic!("checkpoint save finished before the mutation barrier: {result:?}");
        }
        () = wait_for_blocked_query(
            &database.pool,
            "INSERT INTO elitea_runtime.agent_graph_checkpoints",
        ) => {}
    }

    first_lease.revoke();

    barrier
        .commit()
        .await
        .expect("release checkpoint mutation barrier");
    let save_error = tokio::time::timeout(Duration::from_secs(5), save_task)
        .await
        .expect("bounded checkpoint save race")
        .expect("checkpoint save task")
        .expect_err("lease loss before commit must roll back the checkpoint");
    assert_eq!(
        save_error.to_string(),
        concat!(
            "Checkpoint error: checkpoint.writer_not_current: ",
            "the PostgreSQL checkpoint writer is no longer current"
        )
    );
    sqlx::raw_sql(
        r"
DROP TRIGGER block_agent_graph_checkpoint_insert
    ON elitea_runtime.agent_graph_checkpoints;
DROP FUNCTION block_agent_graph_checkpoint_insert();
        ",
    )
    .execute(&database.pool)
    .await
    .expect("remove checkpoint mutation barrier");
    assert!(first.load("thread-1").await.is_err());

    let replacement_lease = Arc::new(TestStateWriterLease::current());
    let replacement = PostgresCheckpointer::activate(
        database.pool.clone(),
        writer("execution-2", "claim-2", 2, 2, [0x41; 32], [0x62; 32]),
        CheckpointLimits::default(),
        replacement_lease.clone(),
    )
    .await
    .expect("activate replacement checkpoint writer");
    assert!(first.load("thread-1").await.is_err());
    assert!(
        replacement
            .load_by_id("checkpoint-race")
            .await
            .expect("look up rolled-back checkpoint")
            .is_none()
    );
    assert_eq!(
        replacement
            .prune("thread-1", &RetentionPolicy::keep_last(1))
            .await
            .expect("prune checkpoint history"),
        2
    );
    assert_eq!(
        replacement
            .list("thread-1")
            .await
            .expect("list retained checkpoint")
            .len(),
        1
    );
    replacement
        .delete("thread-1")
        .await
        .expect("delete checkpoint history");
    assert!(
        replacement
            .list("thread-1")
            .await
            .expect("list empty thread")
            .is_empty()
    );

    let connection = database
        .pool
        .acquire()
        .await
        .expect("reuse pooled connection");
    drop(connection);
    assert!(database.pool.size() <= 4);
    replacement_lease.revoke();
    assert!(replacement.load("thread-1").await.is_err());
}

#[tokio::test]
async fn postgres_application_subgraph_threads_are_admitted_fenced_and_durable() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL checkpoint component test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let lease = Arc::new(TestStateWriterLease::current());
    let root = PostgresCheckpointer::activate(
        database.pool.clone(),
        writer("execution-1", "claim-1", 1, 1, [0x41; 32], [0x61; 32]),
        CheckpointLimits::default(),
        lease.clone(),
    )
    .await
    .expect("activate root");
    assert!(root.load("thread-1/delegate").await.is_err());
    let family: Arc<dyn Checkpointer> = Arc::new(
        root.with_application_children(["delegate"].into_iter())
            .await
            .expect("activate admitted child"),
    );
    for thread in ["other", "thread-1/unknown", "thread-1/delegate/deeper"] {
        assert!(family.load(thread).await.is_err());
        assert!(
            family
                .save(&Checkpoint::new(thread, State::new(), 0, vec![]))
                .await
                .is_err()
        );
    }
    let calls = Arc::new(AtomicUsize::new(0));
    let count = calls.clone();
    let child = StateGraph::with_channels(&["result"])
        .add_node_fn("work", move |_| {
            let count = count.clone();
            async move {
                count.fetch_add(1, Ordering::SeqCst);
                Ok(NodeOutput::new().with_update("result", json!("child completed")))
            }
        })
        .add_edge(START, "work")
        .add_edge("work", END)
        .compile()
        .expect("compile child")
        .with_checkpointer_arc(family.clone());
    let parent = StateGraph::with_channels(&["result"])
        .add_node(adk_rust::graph::subgraph::SubgraphNode::new(
            "delegate",
            Arc::new(child),
        ))
        .add_edge(START, "delegate")
        .add_edge("delegate", END)
        .compile()
        .expect("compile parent")
        .with_checkpointer_arc(family.clone());
    let result = parent
        .invoke(State::new(), ExecutionConfig::new("thread-1"))
        .await
        .expect("execute admitted subgraph");
    assert_eq!(result.get("result"), Some(&json!("child completed")));
    let child_checkpoint = family
        .load("thread-1/delegate")
        .await
        .expect("load child")
        .expect("child saved");
    assert_eq!(
        family
            .load_by_id(&child_checkpoint.checkpoint_id)
            .await
            .expect("load by id")
            .expect("saved")
            .thread_id,
        "thread-1/delegate"
    );
    let replacement = PostgresCheckpointer::activate(
        database.pool.clone(),
        writer("execution-1", "claim-2", 2, 2, [0x41; 32], [0x62; 32]),
        CheckpointLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("take over root")
    .with_application_children(["delegate"].into_iter())
    .await
    .expect("take over child");
    assert!(family.save(&child_checkpoint).await.is_err());
    let recovered = replacement
        .load("thread-1/delegate")
        .await
        .expect("recover child")
        .expect("checkpoint");
    assert_eq!(
        recovered.state.get("result"),
        Some(&json!("child completed"))
    );
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    lease.revoke();
    assert!(family.load("thread-1/delegate").await.is_err());
}

#[cfg(feature = "sandbox-supervisor")]
#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn sandbox_receipts_fence_stale_owners_and_preserve_terminal_results() {
    use crate::sandbox::ledger::{JobLedger, JobScope, LedgerError, Phase};
    let database_url = env::var(TEST_DATABASE_URL)
        .expect("ELITEA_TEST_DATABASE_URL is required for sandbox ledger verification");
    let isolated = IsolatedPostgres::create(&database_url).await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&isolated.pool)
        .await
        .unwrap();
    sqlx::raw_sql(include_str!(
        "../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"
    ))
    .execute(&isolated.pool)
    .await
    .unwrap();
    sqlx::raw_sql(include_str!(
        "../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"
    ))
    .execute(&isolated.pool)
    .await
    .unwrap();
    let ledger = JobLedger::new(isolated.pool.clone());
    let scope = JobScope::new("sandbox-test".into(), 2, [1; 32], [2; 32]).unwrap();
    assert_eq!(ledger.reserve(&scope).await.unwrap().phase, Phase::Reserved);
    let conflict = JobScope::new("sandbox-test".into(), 2, [1; 32], [3; 32]).unwrap();
    assert!(matches!(
        ledger.reserve(&conflict).await,
        Err(LedgerError::Conflict)
    ));
    let foreign = JobScope::new("other-tenant".into(), 2, [1; 32], [2; 32]).unwrap();
    assert!(matches!(
        ledger.read(&foreign).await,
        Err(LedgerError::Missing)
    ));
    let (a, b) = tokio::join!(
        ledger.claim(&scope, "supervisor-a".into(), 30),
        ledger.claim(&scope, "supervisor-b".into(), 30)
    );
    let (a, b) = (a.unwrap(), b.unwrap());
    assert_ne!(a.is_some(), b.is_some());
    let old = a.or(b).unwrap();
    assert!(matches!(
        ledger
            .finish(&old, Phase::Completed, Some("{}"), None)
            .await,
        Err(LedgerError::Fenced)
    ));
    ledger.renew(&old, 30).await.unwrap();
    ledger.mark_dispatched(&old).await.unwrap();
    assert!(matches!(
        ledger.mark_dispatched(&old).await,
        Err(LedgerError::Fenced)
    ));
    // Expire only the isolated test row, without sleeping or changing server time.
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second'",
    )
    .execute(&isolated.pool)
    .await
    .unwrap();
    let recovered = JobLedger::new(isolated.pool.clone());
    let current = recovered
        .claim(&scope, "supervisor-recovered".into(), 30)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(current.observed_phase, Phase::Dispatched);
    assert!(matches!(
        ledger.renew(&old, 30).await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        ledger
            .finish(&old, Phase::Completed, Some("{}"), None)
            .await,
        Err(LedgerError::Fenced)
    ));
    assert!(matches!(
        recovered.mark_dispatched(&current).await,
        Err(LedgerError::Fenced)
    ));
    let result = r#"{"result":{"value":42}}"#;
    recovered
        .finish(&current, Phase::Completed, Some(result), None)
        .await
        .unwrap();
    let terminal = recovered.reserve(&scope).await.unwrap();
    assert_eq!(terminal.phase, Phase::Completed);
    assert_eq!(terminal.result_json.as_deref(), Some(result));
    assert!(
        recovered
            .claim(&scope, "another-owner".into(), 30)
            .await
            .unwrap()
            .is_none()
    );
    assert!(matches!(
        recovered
            .finish(&current, Phase::Failed, None, Some("sandbox.failure"))
            .await,
        Err(LedgerError::Fenced)
    ));

    let uncertain_scope = JobScope::new("sandbox-test".into(), 2, [4; 32], [5; 32]).unwrap();
    ledger.reserve(&uncertain_scope).await.unwrap();
    let lease = ledger
        .claim(&uncertain_scope, "owner".into(), 30)
        .await
        .unwrap()
        .unwrap();
    ledger.mark_dispatched(&lease).await.unwrap();
    ledger
        .finish(
            &lease,
            Phase::Uncertain,
            None,
            Some("sandbox.completion_unknown"),
        )
        .await
        .unwrap();
    assert_eq!(
        ledger.reserve(&uncertain_scope).await.unwrap().phase,
        Phase::Uncertain
    );
    assert!(
        ledger
            .claim(&uncertain_scope, "retry".into(), 30)
            .await
            .unwrap()
            .is_none()
    );
    // Stop before dispatch survives a new ledger instance and forbids execution.
    let stop_scope = JobScope::new("sandbox-test".into(), 2, [6; 32], [7; 32]).unwrap();
    ledger.request_cancellation(&stop_scope).await.unwrap();
    let recovered = JobLedger::new(isolated.pool.clone());
    assert!(recovered.cancellation_requested(&stop_scope).await.unwrap());
    let stopped = recovered
        .claim(&stop_scope, "stop-owner".into(), 30)
        .await
        .unwrap()
        .unwrap();
    assert!(matches!(
        recovered.mark_dispatched(&stopped).await,
        Err(LedgerError::Fenced)
    ));
    recovered
        .finish(&stopped, Phase::Cancelled, None, Some("sandbox.cancelled"))
        .await
        .unwrap();
    assert_eq!(
        recovered.read(&stop_scope).await.unwrap().phase,
        Phase::Cancelled
    );
    // A late stop cannot replace a completed result.
    assert_eq!(
        ledger
            .request_cancellation(&scope)
            .await
            .unwrap()
            .result_json
            .as_deref(),
        Some(result)
    );
    assert!(!ledger.cancellation_requested(&scope).await.unwrap());
    // Completion and stop contend on the same row. Exactly one outcome wins.
    let race = JobScope::new("sandbox-test".into(), 2, [8; 32], [9; 32]).unwrap();
    ledger.reserve(&race).await.unwrap();
    let lease = ledger
        .claim(&race, "race-owner".into(), 30)
        .await
        .unwrap()
        .unwrap();
    ledger.mark_dispatched(&lease).await.unwrap();
    let (finish, cancel) = tokio::join!(
        ledger.finish(&lease, Phase::Completed, Some(result), None),
        ledger.request_cancellation(&race),
    );
    cancel.unwrap();
    if finish.is_ok() {
        assert_eq!(ledger.read(&race).await.unwrap().phase, Phase::Completed);
        assert!(!ledger.cancellation_requested(&race).await.unwrap());
    } else {
        assert!(matches!(finish, Err(LedgerError::Fenced)));
        assert!(ledger.cancellation_requested(&race).await.unwrap());
        ledger
            .finish(&lease, Phase::Cancelled, None, Some("sandbox.cancelled"))
            .await
            .unwrap();
    }
    isolated.pool.close().await;
}

#[cfg(feature = "sandbox-supervisor")]
#[tokio::test]
#[ignore = "requires disposable PostgreSQL, Docker, and ELITEA_CODE_RUNNER_TEST_IMAGE"]
async fn sandbox_supervisor_recovers_dispatched_job_and_persists_before_cleanup() {
    use crate::sandbox::{
        docker_supervisor::{DockerSupervisor, Reconciliation},
        ledger::{JobLedger, JobScope, Phase},
    };
    use adk_sandbox::workspace::{DockerClient, Manifest, ManifestEntry};
    let isolated = IsolatedPostgres::create(&env::var(TEST_DATABASE_URL).unwrap()).await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&isolated.pool)
        .await
        .unwrap();
    sqlx::raw_sql(include_str!(
        "../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"
    ))
    .execute(&isolated.pool)
    .await
    .unwrap();
    sqlx::raw_sql(include_str!(
        "../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"
    ))
    .execute(&isolated.pool)
    .await
    .unwrap();
    let image = env::var("ELITEA_CODE_RUNNER_TEST_IMAGE").unwrap();
    let runtime = DockerClient::with_image(image.clone())
        .await
        .unwrap()
        .with_resource_limits(Some(64 * 1024 * 1024), Some(0.25))
        .with_code_job_policy(Duration::from_secs(15))
        .unwrap();
    let unique = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap()
        .as_nanos();
    let scope = JobScope::new(format!("supervisor-test-{unique}"), 2, [7; 32], [8; 32]).unwrap();
    let identity = scope.runtime_identity().unwrap();
    let ledger = JobLedger::new(isolated.pool.clone());
    ledger.reserve(&scope).await.unwrap();
    let lease = ledger
        .claim(&scope, "old-process".into(), 60)
        .await
        .unwrap()
        .unwrap();
    let request = json!({"argv":["python","-c","import time;time.sleep(1);print('durable-result')"],"timeout_seconds":5});
    runtime
        .provision_code_job(
            &identity,
            &Manifest::new(vec![ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: serde_json::to_vec(&request).unwrap(),
            }]),
        )
        .await
        .unwrap();
    // Persist dispatch before signaling the runtime, then simulate expired ownership.
    ledger.mark_dispatched(&lease).await.unwrap();
    runtime.dispatch_code_job(&identity).await.unwrap();
    sqlx::query(
        "UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second'",
    )
    .execute(&isolated.pool)
    .await
    .unwrap();
    let supervisor = DockerSupervisor::new(
        JobLedger::new(isolated.pool.clone()),
        DockerClient::with_image(image).await.unwrap(),
        "recovered-process".into(),
        2,
    )
    .unwrap();
    let outcome = tokio::time::timeout(
        Duration::from_secs(20),
        supervisor.reconcile_dispatched(&scope),
    )
    .await
    .unwrap()
    .unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = outcome
    else {
        panic!("expected terminal receipt")
    };
    assert!(!cleanup_pending);
    assert_eq!(record.phase, Phase::Completed);
    let receipt: serde_json::Value =
        serde_json::from_str(record.result_json.as_deref().unwrap()).unwrap();
    assert_eq!(receipt["stdout"], "durable-result\n");
    assert!(runtime.observe_code_job(&identity).await.unwrap().is_none());
    assert_eq!(
        ledger.read(&scope).await.unwrap().result_json,
        record.result_json
    );
    assert!(matches!(
        supervisor.reconcile_dispatched(&scope).await.unwrap(),
        Reconciliation::Terminal {
            cleanup_pending: false,
            ..
        }
    ));
    assert!(ledger.renew(&lease, 60).await.is_err());
    // Crash after the database dispatch commit but before the Docker signal.
    // Recovery must execute the prepared runtime, not wait until its deadline.
    let unsignaled =
        JobScope::new(format!("supervisor-test-{unique}"), 2, [11; 32], [12; 32]).unwrap();
    let unsignaled_identity = unsignaled.runtime_identity().unwrap();
    ledger.reserve(&unsignaled).await.unwrap();
    let unsignaled_lease = ledger
        .claim(&unsignaled, "before-signal".into(), 60)
        .await
        .unwrap()
        .unwrap();
    runtime
        .provision_code_job(
            &unsignaled_identity,
            &Manifest::new(vec![ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: serde_json::to_vec(&request).unwrap(),
            }]),
        )
        .await
        .unwrap();
    ledger.mark_dispatched(&unsignaled_lease).await.unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE phase='dispatched'")
        .execute(&isolated.pool).await.unwrap();
    let recovered = tokio::time::timeout(
        Duration::from_secs(20),
        supervisor.reconcile_dispatched(&unsignaled),
    )
    .await
    .unwrap()
    .unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = recovered
    else {
        panic!("expected recovered dispatch receipt")
    };
    assert_eq!(record.phase, Phase::Completed);
    assert!(!cleanup_pending);
    let receipt: serde_json::Value =
        serde_json::from_str(record.result_json.as_deref().unwrap()).unwrap();
    assert_eq!(receipt["stdout"], "durable-result\n");
    assert!(
        runtime
            .observe_code_job(&unsignaled_identity)
            .await
            .unwrap()
            .is_none()
    );
    assert!(ledger.renew(&unsignaled_lease, 60).await.is_err());
    // A durable stop survives owner loss before the execution signal.
    let stopped =
        JobScope::new(format!("supervisor-test-{unique}"), 2, [21; 32], [22; 32]).unwrap();
    let stopped_identity = stopped.runtime_identity().unwrap();
    ledger.reserve(&stopped).await.unwrap();
    runtime
        .provision_code_job(
            &stopped_identity,
            &Manifest::new(vec![ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: serde_json::to_vec(&request).unwrap(),
            }]),
        )
        .await
        .unwrap();
    ledger.request_cancellation(&stopped).await.unwrap();
    let stopped_result = supervisor.reconcile_dispatched(&stopped).await.unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = stopped_result
    else {
        panic!("expected confirmed cancellation");
    };
    assert_eq!(record.phase, Phase::Cancelled);
    assert_eq!(record.failure_code.as_deref(), Some("sandbox.cancelled"));
    assert!(!cleanup_pending);
    assert!(
        runtime
            .observe_code_job(&stopped_identity)
            .await
            .unwrap()
            .is_none()
    );
    // Stop a dispatched runtime, then recover with a replacement lease owner.
    let running =
        JobScope::new(format!("supervisor-test-{unique}"), 2, [23; 32], [24; 32]).unwrap();
    let running_identity = running.runtime_identity().unwrap();
    ledger.reserve(&running).await.unwrap();
    let running_lease = ledger
        .claim(&running, "lost-stop-owner".into(), 60)
        .await
        .unwrap()
        .unwrap();
    let slow = json!({"argv":["python","-c","import time;time.sleep(30)"],"timeout_seconds":15});
    runtime
        .provision_code_job(
            &running_identity,
            &Manifest::new(vec![ManifestEntry::File {
                path: ".elitea-job.json".into(),
                content: serde_json::to_vec(&slow).unwrap(),
            }]),
        )
        .await
        .unwrap();
    ledger.mark_dispatched(&running_lease).await.unwrap();
    runtime.dispatch_code_job(&running_identity).await.unwrap();
    ledger.request_cancellation(&running).await.unwrap();
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE phase='dispatched'")
        .execute(&isolated.pool).await.unwrap();
    let outcome = tokio::time::timeout(
        Duration::from_secs(10),
        supervisor.reconcile_dispatched(&running),
    )
    .await
    .unwrap()
    .unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = outcome
    else {
        panic!("expected stopped runtime")
    };
    assert_eq!(record.phase, Phase::Cancelled);
    assert!(!cleanup_pending);
    assert!(
        runtime
            .observe_code_job(&running_identity)
            .await
            .unwrap()
            .is_none()
    );

    // Lost dispatch acknowledgement must not reset the outer job deadline.
    let abandoned =
        JobScope::new(format!("supervisor-test-{unique}"), 2, [9; 32], [10; 32]).unwrap();
    let abandoned_identity = abandoned.runtime_identity().unwrap();
    ledger.reserve(&abandoned).await.unwrap();
    let abandoned_lease = ledger
        .claim(&abandoned, "lost-dispatch".into(), 60)
        .await
        .unwrap()
        .unwrap();
    runtime
        .provision_code_job(&abandoned_identity, &Manifest::new(vec![]))
        .await
        .unwrap();
    ledger.mark_dispatched(&abandoned_lease).await.unwrap();
    // Test-only time adjustment; no signal was sent to the prepared container.
    sqlx::query("UPDATE elitea_runtime.sandbox_jobs SET created_at=clock_timestamp()-interval '2 hours', lease_until=clock_timestamp()-interval '1 second' WHERE phase='dispatched'")
        .execute(&isolated.pool).await.unwrap();
    let deadline = tokio::time::timeout(
        Duration::from_secs(20),
        supervisor.reconcile_dispatched(&abandoned),
    )
    .await
    .unwrap()
    .unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = deadline
    else {
        panic!("expected deadline receipt")
    };
    assert_eq!(record.phase, Phase::Failed);
    assert_eq!(
        record.failure_code.as_deref(),
        Some("sandbox.deadline_exceeded")
    );
    assert!(!cleanup_pending);
    assert!(
        runtime
            .observe_code_job(&abandoned_identity)
            .await
            .unwrap()
            .is_none()
    );
    isolated.pool.close().await;
}

#[cfg(feature = "sandbox-supervisor")]
#[tokio::test]
#[ignore = "requires disposable PostgreSQL, Docker and the test-only adapter image"]
async fn sandbox_supervisor_submits_only_the_authorized_request_once() {
    sandbox_submission(false).await;
}

#[cfg(feature = "sandbox-supervisor")]
#[tokio::test]
#[ignore = "requires disposable PostgreSQL, Docker, TLS fixtures and a cached adapter image"]
async fn sandbox_supervisor_mtls_submission() {
    sandbox_submission(true).await;
}

#[cfg(feature = "sandbox-supervisor")]
async fn sandbox_submission(over_tls: bool) {
    use crate::{
        protocol::{
            command::Ed25519PublicKeyResolver,
            elitea::runtime::v1::{SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1},
            sandbox_grant::GrantVerifier,
        },
        sandbox::{
            docker_supervisor::{DockerSupervisor, Reconciliation},
            ledger::{JobLedger, LedgerError, Phase},
            request::{Language, PreparedJob},
        },
    };
    use adk_sandbox::workspace::DockerClient;
    use prost::Message;
    use ring::signature::{Ed25519KeyPair, KeyPair};
    struct Keys([u8; 32]);
    impl Ed25519PublicKeyResolver for Keys {
        fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
            (id == "fixture-key").then_some(self.0)
        }
    }
    let isolated = IsolatedPostgres::create(&env::var(TEST_DATABASE_URL).unwrap()).await;
    sqlx::raw_sql("CREATE SCHEMA elitea_runtime")
        .execute(&isolated.pool)
        .await
        .unwrap();
    sqlx::raw_sql(include_str!(
        "../../../elitea-main/migrations/agentstate/0004_sandbox_jobs.sql"
    ))
    .execute(&isolated.pool)
    .await
    .unwrap();
    sqlx::raw_sql(include_str!(
        "../../../elitea-main/migrations/agentstate/0005_sandbox_cancellation.sql"
    ))
    .execute(&isolated.pool)
    .await
    .unwrap();
    let image = env::var("ELITEA_CODE_RUNNER_TEST_IMAGE").unwrap();
    let compiled = env::var("ELITEA_TEST_ADAPTER_LANGUAGE").as_deref() == Ok("rust");
    let language = if compiled {
        Language::Rust
    } else {
        Language::Python
    };
    let source = if compiled {
        "pub fn run(_: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> { Ok(serde_json::json!({\"marker\":format!(\"authorized-result-{}\", std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?.as_nanos())})) }"
    } else {
        "import uuid\nresult = 'authorized-result-' + str(uuid.uuid4())\nprint(result)\n{'marker': result}"
    };
    let request = PreparedJob::new(
        language,
        source.into(),
        Default::default(),
        image.clone(),
        "fixture-v1".into(),
        15,
    )
    .unwrap();
    let changed = PreparedJob::new(
        Language::Python,
        "print('different')".into(),
        Default::default(),
        image.clone(),
        "fixture-v1".into(),
        5,
    )
    .unwrap();
    let now = i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_millis(),
    )
    .unwrap();
    let key = Ed25519KeyPair::from_seed_unchecked(&[7; 32]).unwrap();
    let mut public = [0; 32];
    public.copy_from_slice(key.public_key().as_ref());
    let claims = SandboxJobGrantClaimsV1 {
        revision: 1,
        tenant_id: "fixture-tenant".into(),
        project_id: 2,
        execution_id: format!("fixture-execution-{now}"),
        activation_id: "node-1".into(),
        request_digest: request.fingerprint().unwrap().to_vec(),
        submitter_workload_identity: if over_tls {
            "dns:worker.test"
        } else {
            "fixture-worker"
        }
        .into(),
        audience: "fixture-supervisor".into(),
        issued_at_unix_millis: now,
        expires_at_unix_millis: now + 30000,
        generation: 1,
    };
    let bytes = claims.encode_to_vec();
    let mut signing = b"elitea.sandbox.job-grant.ed25519.v1\0".to_vec();
    signing.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    signing.extend_from_slice(&bytes);
    let grant = SignedSandboxJobGrantV1 {
        key_id: "fixture-key".into(),
        claims_bytes: bytes,
        signature: key.sign(&signing).as_ref().to_vec(),
    };
    let authorized = GrantVerifier::new(Keys(public), "fixture-supervisor".into())
        .unwrap()
        .verify(
            &grant,
            if over_tls {
                "dns:worker.test"
            } else {
                "fixture-worker"
            },
            &request,
            now,
        )
        .unwrap();
    let runtime = DockerClient::with_image(image.clone())
        .await
        .unwrap()
        .with_resource_limits(Some(512 * 1024 * 1024), Some(1.0))
        .with_code_job_policy(Duration::from_secs(15))
        .unwrap();
    let runtime = if compiled {
        runtime.with_code_compilation().unwrap()
    } else {
        runtime
    };
    let supervisor = DockerSupervisor::new(
        JobLedger::new(isolated.pool.clone()),
        runtime,
        "fixture-owner".into(),
        2,
    )
    .unwrap();
    let ledger = JobLedger::new(isolated.pool.clone());
    assert!(
        supervisor
            .submit_authorized(&authorized, &request)
            .await
            .is_err()
    );
    assert!(matches!(
        ledger.read(authorized.scope()).await,
        Err(LedgerError::Missing)
    ));
    let supervisor = supervisor
        .with_admission_policy("fixture-v1".into(), vec![language])
        .unwrap();
    assert!(
        supervisor
            .submit_authorized(&authorized, &changed)
            .await
            .is_err()
    );
    assert!(matches!(
        ledger.read(authorized.scope()).await,
        Err(LedgerError::Missing)
    ));
    if over_tls {
        crate::diagnostics::install_tls_crypto_provider().unwrap();
        use crate::protocol::elitea::runtime::v1::{
            SubmitSandboxJobRequestV1,
            sandbox_supervisor_service_client::SandboxSupervisorServiceClient,
        };
        use crate::sandbox::service::SupervisorService;
        use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};
        let dir = std::path::PathBuf::from(env::var("ELITEA_TEST_TLS_DIR").unwrap());
        let read = |name: &str| std::fs::read(dir.join(name)).unwrap();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let service = SupervisorService::new(
            GrantVerifier::new(Keys(public), "fixture-supervisor".into()).unwrap(),
            std::sync::Arc::new(supervisor),
        );
        let (stop, stopped) = tokio::sync::oneshot::channel();
        let mut tasks = tokio::task::JoinSet::new();
        tasks.spawn(service.serve(
            listener,
            Identity::from_pem(read("server.pem"), read("server.key")),
            Certificate::from_pem(read("ca.pem")),
            async {
                let _ = stopped.await;
            },
        ));
        let endpoint = Endpoint::from_shared(format!("https://{address}"))
            .unwrap()
            .connect_timeout(Duration::from_secs(5))
            .timeout(Duration::from_secs(25));
        let wire = SubmitSandboxJobRequestV1 {
            grant: Some(grant.clone()),
            prepared_job_json: request.to_transport().unwrap(),
        };
        // A trusted CA alone does not authenticate the caller.
        let anonymous = endpoint
            .clone()
            .tls_config(
                ClientTlsConfig::new()
                    .ca_certificate(Certificate::from_pem(read("ca.pem")))
                    .domain_name("supervisor.test"),
            )
            .unwrap()
            .connect()
            .await;
        if let Ok(channel) = anonymous {
            assert!(
                SandboxSupervisorServiceClient::new(channel)
                    .submit_sandbox_job(wire.clone())
                    .await
                    .is_err()
            );
        }
        let connect = |name: &str| {
            endpoint
                .clone()
                .tls_config(
                    ClientTlsConfig::new()
                        .ca_certificate(Certificate::from_pem(read("ca.pem")))
                        .domain_name("supervisor.test")
                        .identity(Identity::from_pem(
                            read(&format!("{name}.pem")),
                            read(&format!("{name}.key")),
                        )),
                )
                .unwrap()
        };
        let mut wrong =
            SandboxSupervisorServiceClient::new(connect("other").connect().await.unwrap());
        assert_eq!(
            wrong
                .submit_sandbox_job(wire.clone())
                .await
                .unwrap_err()
                .code(),
            tonic::Code::PermissionDenied
        );
        let worker_channel = connect("worker").connect().await.unwrap();
        let mut client = SandboxSupervisorServiceClient::new(worker_channel.clone());
        let mut altered = wire.clone();
        altered.prepared_job_json = changed.to_transport().unwrap();
        assert_eq!(
            client.submit_sandbox_job(altered).await.unwrap_err().code(),
            tonic::Code::PermissionDenied
        );
        assert!(matches!(
            ledger.read(authorized.scope()).await,
            Err(LedgerError::Missing)
        ));
        use crate::sandbox::client::{SandboxClient, SandboxOutcome};
        let transport = SandboxClient::from_channel(
            worker_channel,
            "fixture-supervisor".into(),
            Duration::from_secs(25),
        )
        .unwrap();
        let SandboxOutcome::Completed(result) = transport
            .submit_granted(grant.clone(), &request)
            .await
            .unwrap()
        else {
            panic!("completed receipt expected");
        };
        let receipt: serde_json::Value = serde_json::from_slice(&result).unwrap();
        let output: serde_json::Value =
            serde_json::from_str(receipt["stdout"].as_str().unwrap()).unwrap();
        assert!(
            output["result"]["marker"]
                .as_str()
                .unwrap()
                .starts_with("authorized-result-")
        );
        let SandboxOutcome::Completed(repeated) =
            transport.submit_granted(grant, &request).await.unwrap()
        else {
            panic!("saved receipt expected");
        };
        assert_eq!(repeated, result);
        drop(transport);
        stop.send(()).unwrap();
        drop(client);
        drop(wrong);
        tokio::time::timeout(Duration::from_secs(5), tasks.join_next())
            .await
            .unwrap()
            .unwrap()
            .unwrap()
            .unwrap();
        isolated.pool.close().await;
        return;
    }
    let result = tokio::time::timeout(
        Duration::from_secs(20),
        supervisor.submit_authorized(&authorized, &request),
    )
    .await
    .unwrap()
    .unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = result
    else {
        panic!("terminal receipt expected")
    };
    assert_eq!(record.phase, Phase::Completed);
    assert!(!cleanup_pending);
    let stored = record.result_json.unwrap();
    assert!(stored.contains("authorized-result-"));
    if env::var("ELITEA_TEST_REAL_ADAPTER").as_deref() == Ok("1") {
        let receipt: serde_json::Value = serde_json::from_str(&stored).unwrap();
        let output: serde_json::Value =
            serde_json::from_str(receipt["stdout"].as_str().unwrap()).unwrap();
        assert_eq!(output["revision"], 1);
        assert!(
            output["result"]["marker"]
                .as_str()
                .unwrap()
                .starts_with("authorized-result-")
        );
    }
    let repeated = supervisor
        .submit_authorized(&authorized, &request)
        .await
        .unwrap();
    let Reconciliation::Terminal {
        record,
        cleanup_pending,
    } = repeated
    else {
        panic!("saved receipt expected")
    };
    assert_eq!(record.result_json.as_deref(), Some(stored.as_str()));
    assert!(!cleanup_pending);
    let observer = DockerClient::with_image(image).await.unwrap();
    assert!(
        observer
            .observe_code_job(&authorized.scope().runtime_identity().unwrap())
            .await
            .unwrap()
            .is_none()
    );
    isolated.pool.close().await;
}
