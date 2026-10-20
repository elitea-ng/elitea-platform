//! FU-214 against real `PostgreSQL`: a Code attempt whose sandbox runtime was
//! lost is re-run once per platform allowance behind the fenced node journal,
//! across process replacement and a second claim, and completes exactly once.
//! A user-code failure and an exhausted allowance stay typed stops.
use super::*;
use crate::agents::graph::node_recovery::{
    MAX_RUNTIME_LOST_RETRIES, NodeFailure, NodeFailureClass, NodeRecoveryPolicy, ReplaySafety,
};
use crate::agents::graph::node_recovery_runtime::{
    NodeAttemptAuthority, NodeAttemptBody, NodeRecoveryFactory, RecoverableNode,
};
use adk_rust::graph::{Node, NodeContext, NodeOutput};
use std::collections::VecDeque;
use tokio::sync::Notify;

const JOURNAL_KEY: &str = "elitea.pipeline.node-attempt-journal.v1";
const WAIT: Duration = Duration::from_secs(20);
const LEGACY_DISPATCH: [u8; 32] = [9; 32];

#[derive(Clone, Copy)]
enum Outcome {
    Lost,
    UserError,
    Succeed,
    /// Block inside the attempt until released, then succeed.
    Gated,
}

#[derive(Default)]
struct Gate {
    entered: Notify,
    release: Notify,
}

/// A Code-like attempt body: no sandbox, the scripted outcome of each attempt.
struct Body {
    outcomes: std::sync::Mutex<VecDeque<Outcome>>,
    calls: std::sync::Mutex<Vec<(u16, [u8; 32], bool)>>,
    gate: Arc<Gate>,
}

impl Body {
    fn new(outcomes: &[Outcome]) -> Arc<Self> {
        Arc::new(Self {
            outcomes: std::sync::Mutex::new(outcomes.iter().copied().collect()),
            calls: std::sync::Mutex::new(Vec::new()),
            gate: Arc::new(Gate::default()),
        })
    }
    fn calls(&self) -> Vec<(u16, [u8; 32], bool)> {
        self.calls.lock().unwrap().clone()
    }
}

#[async_trait::async_trait]
impl Node for Body {
    #[allow(
        clippy::unnecessary_literal_bound,
        reason = "Match the runtime trait signature in this fixture."
    )]
    fn name(&self) -> &str {
        "code"
    }
    async fn execute(&self, _: &NodeContext) -> Result<NodeOutput, adk_rust::graph::GraphError> {
        Err(crate::agents::graph::node_recovery_runtime::recovery_error(
            "test.raw_execute_forbidden",
        ))
    }
}

#[async_trait::async_trait]
impl NodeAttemptBody for Body {
    fn legacy_dispatch_identity(
        &self,
        _: &NodeContext,
    ) -> Result<Option<[u8; 32]>, adk_rust::graph::GraphError> {
        Ok(Some(LEGACY_DISPATCH))
    }
    async fn execute_attempt(
        &self,
        _: &NodeContext,
        authority: &NodeAttemptAuthority,
    ) -> Result<NodeOutput, NodeFailure> {
        self.calls.lock().unwrap().push((
            authority.attempt(),
            authority.dispatch_activation(),
            authority.recovering_started(),
        ));
        let outcome = self
            .outcomes
            .lock()
            .unwrap()
            .pop_front()
            .expect("scripted attempt outcome");
        match outcome {
            Outcome::Lost => Err(NodeFailure::new(
                NodeFailureClass::RuntimeLost,
                ReplaySafety::NoExternalEffect,
            )),
            Outcome::UserError => Err(NodeFailure::new(
                NodeFailureClass::InvalidResult,
                ReplaySafety::NoExternalEffect,
            )),
            Outcome::Succeed => Ok(NodeOutput::new().with_update("count", json!(41))),
            Outcome::Gated => {
                self.gate.entered.notify_one();
                self.gate.release.notified().await;
                Ok(NodeOutput::new().with_update("count", json!(41)))
            }
        }
    }
}

fn node(factory: Arc<dyn NodeRecoveryFactory>, body: Arc<Body>) -> Arc<dyn Node> {
    // A default-policy Code node: the production compiler wraps it like this.
    Arc::new(
        RecoverableNode::new(body, [2; 32], NodeRecoveryPolicy::default(), factory, None)
            .with_legacy_first_attempt(),
    )
}

fn context() -> NodeContext {
    NodeContext::new(
        State::from([("value".to_owned(), json!(7))]),
        ExecutionConfig::new("thread-1"),
        2,
    )
}

async fn database() -> IsolatedPostgres {
    let url = crate::test_pg::database_url_var(TEST_DATABASE_URL)
        .expect("ELITEA_TEST_DATABASE_URL is required");
    let database = IsolatedPostgres::create(&url).await;
    install_test_schema(&database.pool).await;
    database
}

async fn fresh_pool(database: &IsolatedPostgres) -> PgPool {
    crate::test_pg::pool_options()
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

async fn checkpointer(pool: PgPool, claim_attempt: u64) -> Arc<PostgresCheckpointer> {
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
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .expect("activate the node journal writer"),
    )
}

/// The decoded ledger events of every journal row, oldest first, with the
/// writer claim of each row.
async fn journal(pool: &PgPool) -> Vec<(String, Vec<String>, bool)> {
    let rows: Vec<(String, serde_json::Value, String)> = sqlx::query_as(
        r"
SELECT writer_claim_id, metadata::jsonb -> $1, coalesce(
       jsonb_typeof(metadata::jsonb -> $1 -> 'completed_updates'), 'null')
FROM elitea_runtime.agent_graph_checkpoints
WHERE thread_id LIKE 'n1:%'
ORDER BY thread_id, save_ordinal
        ",
    )
    .bind(JOURNAL_KEY)
    .fetch_all(pool)
    .await
    .expect("read node-attempt journal rows");
    rows.into_iter()
        .map(|(claim, record, completed)| {
            let kinds = record["ledger"]["ledger"]["events"]
                .as_array()
                .expect("ledger events")
                .iter()
                .map(|event| event["kind"].as_str().unwrap().to_owned())
                .collect();
            (claim, kinds, completed != "null")
        })
        .collect()
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_lost_runtime_is_rerun_across_process_replacement_and_settles_once() {
    let database = database().await;
    let first_pool = fresh_pool(&database).await;
    let claim_one = checkpointer(first_pool.clone(), 1).await;
    let first_body = Body::new(&[Outcome::Lost, Outcome::Gated]);
    let first = node(claim_one.clone(), first_body.clone());
    let running = {
        let first = Arc::clone(&first);
        tokio::spawn(async move { first.execute(&context()).await.map(|_| ()) })
    };
    // Claim 1 records the lost runtime, waits out the re-run delay and starts
    // attempt 2. The process dies inside the re-run.
    tokio::time::timeout(WAIT, first_body.gate.entered.notified())
        .await
        .expect("the lost runtime was re-run");
    running.abort();
    assert!(running.await.unwrap_err().is_cancelled());
    drop(first);
    drop(claim_one);
    first_pool.close().await;
    let calls = first_body.calls();
    assert_eq!(calls.len(), 2);
    // Attempt 1 keeps the default-policy identity; the re-run is a new ordinal
    // identity, so the supervisor and Main see a separate job and visit.
    assert_eq!(calls[0], (1, LEGACY_DISPATCH, false));
    assert_eq!(calls[1].0, 2);
    assert_ne!(calls[1].1, LEGACY_DISPATCH);
    assert!(!calls[1].2);
    let rows = journal(&database.pool).await;
    assert_eq!(
        rows.len(),
        3,
        "Started, the lost failure, the re-run's Started"
    );
    assert_eq!(
        rows.last().unwrap().1,
        ["start", "failure", "start"],
        "{rows:?}"
    );

    // Claim 2 in a replacement process reconciles the started re-run with the
    // same identity and completes it once.
    let claim_two = checkpointer(fresh_pool(&database).await, 2).await;
    let second_body = Body::new(&[Outcome::Succeed]);
    let second = node(claim_two, second_body.clone());
    let output = second.execute(&context()).await.unwrap();
    assert_eq!(output.updates["count"], json!(41));
    assert_eq!(second_body.calls(), [(2, calls[1].1, true)]);
    let rows = journal(&database.pool).await;
    assert_eq!(rows.len(), 4);
    assert_eq!(
        rows.last().unwrap().1,
        ["start", "failure", "start", "success"]
    );
    assert!(rows.last().unwrap().2, "the completed result is durable");
    assert_eq!(
        rows.iter().map(|row| row.0.as_str()).collect::<Vec<_>>(),
        ["claim-1", "claim-1", "claim-1", "claim-2"]
    );
    // Re-entry projects the durable result and never runs the attempt again.
    assert_eq!(
        second.execute(&context()).await.unwrap().updates,
        output.updates
    );
    assert_eq!(second_body.calls().len(), 1);
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_superseded_claim_cannot_settle_its_rerun() {
    let database = database().await;
    let claim_one = checkpointer(database.pool.clone(), 1).await;
    let first_body = Body::new(&[Outcome::Lost, Outcome::Gated]);
    let first = node(claim_one, first_body.clone());
    let running = {
        let first = Arc::clone(&first);
        tokio::spawn(async move { first.execute(&context()).await.map(|_| ()) })
    };
    tokio::time::timeout(WAIT, first_body.gate.entered.notified())
        .await
        .expect("the lost runtime was re-run");
    // Claim 2 takes the run over while claim 1 is inside the re-run.
    let claim_two = checkpointer(database.pool.clone(), 2).await;
    let second_body = Body::new(&[Outcome::Succeed]);
    let second = node(claim_two, second_body.clone());
    assert_eq!(
        second.execute(&context()).await.unwrap().updates["count"],
        json!(41)
    );
    // Claim 1's attempt finishes, but its result cannot pass claim 2's fence.
    first_body.gate.release.notify_one();
    let late = tokio::time::timeout(WAIT, running)
        .await
        .expect("the fenced attempt finished")
        .unwrap();
    assert!(late.is_err(), "the superseded claim settled its re-run");
    let rows = journal(&database.pool).await;
    let completed: Vec<_> = rows.iter().filter(|row| row.2).collect();
    assert_eq!(completed.len(), 1, "{rows:?}");
    assert_eq!(completed[0].0, "claim-2");
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn a_user_code_failure_is_never_rerun() {
    let database = database().await;
    let body = Body::new(&[Outcome::UserError]);
    let node = node(checkpointer(database.pool.clone(), 1).await, body.clone());
    let error = node.execute(&context()).await.err().unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    assert!(node.execute(&context()).await.is_err());
    assert_eq!(body.calls().len(), 1);
    let rows = journal(&database.pool).await;
    assert_eq!(rows.last().unwrap().1, ["start", "failure"]);
    assert!(rows.iter().all(|row| !row.2));
}

#[tokio::test]
#[ignore = "requires isolated PostgreSQL database creation through ELITEA_TEST_DATABASE_URL"]
async fn an_exhausted_allowance_stops_typed_after_the_bounded_reruns() {
    let database = database().await;
    let lost = vec![Outcome::Lost; usize::from(MAX_RUNTIME_LOST_RETRIES) + 1];
    let body = Body::new(&lost);
    let node = node(checkpointer(database.pool.clone(), 1).await, body.clone());
    let error = tokio::time::timeout(WAIT, node.execute(&context()))
        .await
        .expect("the bounded re-runs finished")
        .err()
        .unwrap();
    assert!(error.to_string().contains("pipeline.node_recovery.failed"));
    let calls = body.calls();
    assert_eq!(calls.len(), usize::from(MAX_RUNTIME_LOST_RETRIES) + 1);
    assert_eq!(
        calls.iter().map(|call| call.0).collect::<Vec<_>>(),
        (1..=MAX_RUNTIME_LOST_RETRIES + 1).collect::<Vec<_>>()
    );
    // Every re-run is its own identity.
    let identities = calls
        .iter()
        .map(|call| call.1)
        .collect::<std::collections::BTreeSet<_>>();
    assert_eq!(identities.len(), calls.len());
    // A re-entry stays stopped and dispatches nothing.
    assert!(node.execute(&context()).await.is_err());
    assert_eq!(body.calls().len(), calls.len());
    assert!(journal(&database.pool).await.iter().all(|row| !row.2));
}
