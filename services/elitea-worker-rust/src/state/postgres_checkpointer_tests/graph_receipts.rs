//! Required-mode component proofs for immutable graph-call receipt appends.
//! Root owns execution against the explicitly guarded disposable `PostgreSQL` 18.
use super::*;
use crate::agents::events::APPLICATION_BRANCH_ROOT;
use crate::agents::graph::compiler::PipelineDefinition;
use crate::agents::pipeline::scope_receipts::{
    GraphCallOutcome, PipelineGraphReceiptCheckpointer, finish_graph_call, prepare_graph_call,
    receipt_for_node, receipt_revision_parent,
};
use crate::agents::pipeline::scoped_applications::PipelineApplicationScope;
use adk_rust::graph::{GraphError, NodeContext};
use adk_rust::{Content, Event, Part};
use async_trait::async_trait;
use tokio::sync::Mutex;

const REQUIRE_RECEIPTS: &str = "ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS";
const DATABASE_GUARD: &str = "ELITEA_TEST_DATABASE_GUARD";

async fn required_database() -> Option<IsolatedPostgres> {
    let required = env::var(REQUIRE_RECEIPTS).ok();
    let url = env::var(TEST_DATABASE_URL).ok();
    if url.is_none() && required.is_none() {
        eprintln!(
            "receipt PG proof not run: root must set required mode and disposable database guard"
        );
        return None;
    }
    assert_eq!(
        required.as_deref(),
        Some("1"),
        "receipt PG proofs require explicit required mode"
    );
    assert_eq!(
        env::var(DATABASE_GUARD).ok().as_deref(),
        Some("disposable-pg18"),
        "receipt PG proofs require the disposable-PG18 guard"
    );
    let url = url.expect("required receipt PG proof has no ELITEA_TEST_DATABASE_URL");
    let options = PgConnectOptions::from_str(&url).expect("parse required fixture URL");
    assert!(
        matches!(options.get_host(), "localhost" | "127.0.0.1" | "::1"),
        "required receipt fixture must use the loopback disposable server"
    );
    assert_eq!(
        options.get_database(),
        Some("postgres"),
        "required fixture administrator must target postgres, never a product database"
    );
    let admin = PgPoolOptions::new()
        .max_connections(1)
        .connect_with(options.disable_statement_logging())
        .await
        .expect("connect guarded fixture server");
    let version = sqlx::query_scalar::<_, String>("SHOW server_version_num")
        .fetch_one(&admin)
        .await
        .expect("read fixture server version")
        .parse::<u32>()
        .expect("numeric fixture version");
    assert!(
        (180_000..190_000).contains(&version),
        "required fixture must run on disposable PostgreSQL 18"
    );
    admin.close().await;
    let database = IsolatedPostgres::create(&url).await;
    assert!(database.database_name.starts_with("elitea_rust_cp_"));
    install_test_schema(&database.pool).await;
    Some(database)
}

fn scope() -> PipelineApplicationScope {
    let definition=PipelineDefinition::from_yaml("state: {answer: str}\nentry_point: review\nnodes:\n  - id: review\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: publish\n  - id: publish\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n").unwrap();
    PipelineApplicationScope::new(String::new(), &definition, None).unwrap()
}
fn context() -> NodeContext {
    NodeContext::new(State::new(), ExecutionConfig::new("thread-1"), 2)
}
async fn prepare(
    checkpointer: &dyn Checkpointer,
    node: &str,
) -> crate::agents::pipeline::scope_receipts::PipelineGraphCallReceipt {
    prepare_graph_call(
        checkpointer,
        &Mutex::new(()),
        &context(),
        scope().activate("thread-1", node, 2).unwrap(),
        "assistant",
        &json!({"task":"work"}),
        "original-runtime",
        "graph",
        APPLICATION_BRANCH_ROOT,
    )
    .await
    .unwrap()
    .0
}
fn unchanged_frontier(left: &Checkpoint, right: &Checkpoint) {
    assert_eq!(left.thread_id, right.thread_id);
    assert_eq!(left.state, right.state);
    assert_eq!(left.step, right.step);
    assert_eq!(left.pending_nodes, right.pending_nodes);
    assert_eq!(left.cleared_interrupt, right.cleared_interrupt);
    assert_eq!(left.attempts, right.attempts);
    assert_eq!(left.child_ledger, right.child_ledger);
    assert_eq!(left.metadata.get("route"), right.metadata.get("route"));
}
fn terminal(
    record: &crate::agents::pipeline::scope_receipts::PipelineGraphCallReceipt,
    result: serde_json::Value,
) -> Event {
    let mut terminal = Event::new(record.invocation_id());
    terminal.author = record.author().to_owned();
    terminal.branch = record.branch().to_owned();
    terminal.llm_response.content = Some(Content {
        role: "function".to_owned(),
        parts: vec![Part::FunctionResponse {
            function_response: adk_rust::FunctionResponseData::new("assistant", result),
            id: Some(record.activation().call_id().to_owned()),
            annotations: None,
        }],
    });
    record
        .activation()
        .stamp_with_branch(&mut terminal, record.branch().to_owned())
        .unwrap();
    terminal
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // Keep durable start, pause, completion, replay, and takeover in one ordered proof.
async fn postgres_graph_receipts_begin_pause_complete_replay_takeover_and_stale_writer() {
    let Some(database) = required_database().await else {
        return;
    };
    let first = Arc::new(
        PostgresCheckpointer::activate(
            database.pool.clone(),
            writer("execution-1", "claim-1", 1, 1, [0x41; 32], [0x61; 32]),
            CheckpointLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let wrapped = PipelineGraphReceiptCheckpointer::new(first.clone(), Arc::new(Mutex::new(())));
    let original = checkpoint("original-frontier", "2026-10-02T10:00:00Z", 2);
    first.save(&original).await.unwrap();
    let effects = AtomicUsize::new(0);
    let start = prepare(&wrapped, "review").await;
    let begun = first.load("thread-1").await.unwrap().unwrap();
    assert_ne!(begun.checkpoint_id, original.checkpoint_id);
    assert_eq!(
        receipt_revision_parent(&begun).unwrap(),
        Some(original.checkpoint_id.clone())
    );
    unchanged_frontier(&original, &begun);
    assert!(matches!(
        receipt_for_node(&begun, "review")
            .unwrap()
            .unwrap()
            .outcome(),
        GraphCallOutcome::Started
    ));
    // Simulated child polling starts only after a successful durable begin.
    effects.fetch_add(1, Ordering::SeqCst);
    let ids = std::collections::BTreeSet::from(["leaf-one".to_owned(), "leaf-two".to_owned()]);
    let paused = finish_graph_call(
        &wrapped,
        &Mutex::new(()),
        &context(),
        &start,
        GraphCallOutcome::Paused {
            result: json!({"__elitea_nested_interrupt_v1":ids}),
        },
    )
    .await
    .unwrap();
    let paused_checkpoint = first.load("thread-1").await.unwrap().unwrap();
    assert_ne!(paused_checkpoint.checkpoint_id, begun.checkpoint_id);
    unchanged_frontier(&original, &paused_checkpoint);
    assert_eq!(
        serde_json::to_value(
            first
                .load_by_id(&original.checkpoint_id)
                .await
                .unwrap()
                .unwrap()
        )
        .unwrap(),
        serde_json::to_value(&original).unwrap()
    );
    assert_eq!(
        serde_json::to_value(
            first
                .load_by_id(&begun.checkpoint_id)
                .await
                .unwrap()
                .unwrap()
        )
        .unwrap(),
        serde_json::to_value(&begun).unwrap()
    );
    let replacement = Arc::new(
        PostgresCheckpointer::activate(
            database.pool.clone(),
            writer("execution-2", "claim-2", 2, 2, [0x41; 32], [0x62; 32]),
            CheckpointLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let replacement_wrapped =
        PipelineGraphReceiptCheckpointer::new(replacement.clone(), Arc::new(Mutex::new(())));
    let recovered = prepare(&replacement_wrapped, "review").await;
    assert_eq!(
        serde_json::to_value(&recovered).unwrap(),
        serde_json::to_value(&paused).unwrap()
    );
    assert_eq!(effects.load(Ordering::SeqCst), 1);
    let result = json!({"response":"completed once"});
    let done = finish_graph_call(
        &replacement_wrapped,
        &Mutex::new(()),
        &context(),
        &recovered,
        GraphCallOutcome::Completed {
            result: result.clone(),
            terminal: terminal(&recovered, result),
        },
    )
    .await
    .unwrap();
    let completed_checkpoint = replacement.load("thread-1").await.unwrap().unwrap();
    assert_ne!(
        completed_checkpoint.checkpoint_id,
        paused_checkpoint.checkpoint_id
    );
    unchanged_frontier(&original, &completed_checkpoint);
    assert_eq!(
        serde_json::to_value(&prepare(&replacement_wrapped, "review").await).unwrap(),
        serde_json::to_value(&done).unwrap()
    );
    replacement_wrapped.save(&begun).await.unwrap();
    replacement_wrapped.save(&original).await.unwrap();
    assert_eq!(
        replacement
            .load("thread-1")
            .await
            .unwrap()
            .unwrap()
            .checkpoint_id,
        completed_checkpoint.checkpoint_id
    );
    assert_eq!(effects.load(Ordering::SeqCst), 1);
    assert!(first.save(&original).await.is_err());
    assert!(
        finish_graph_call(
            &wrapped,
            &Mutex::new(()),
            &context(),
            &paused,
            GraphCallOutcome::Completed {
                result: json!({"response":"stale"}),
                terminal: terminal(&paused, json!({"response":"stale"}))
            }
        )
        .await
        .is_err()
    );
    let mut changed = original.clone();
    changed.metadata.insert("changed".to_owned(), json!(true));
    assert!(matches!(
        replacement.save(&changed).await,
        Err(GraphError::CheckpointError(message)) if message.starts_with("checkpoint.conflict:")
    ));
    assert_eq!(
        replacement
            .load("thread-1")
            .await
            .unwrap()
            .unwrap()
            .checkpoint_id,
        completed_checkpoint.checkpoint_id
    );
}

struct CaptureAppend {
    inner: Arc<dyn Checkpointer>,
    candidate: Mutex<Option<Checkpoint>>,
}
#[async_trait]
impl Checkpointer for CaptureAppend {
    async fn save(&self, candidate: &Checkpoint) -> Result<String, GraphError> {
        *self.candidate.lock().await = Some(candidate.clone());
        Ok(candidate.checkpoint_id.clone())
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.inner.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.inner.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.inner.delete(thread).await
    }
    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.inner.prune(thread, policy).await
    }
}
#[tokio::test]
async fn postgres_graph_receipts_append_compares_latest_parent_and_unchanged_frontier_atomically() {
    let Some(database) = required_database().await else {
        return;
    };
    let owner = Arc::new(
        PostgresCheckpointer::activate(
            database.pool.clone(),
            writer("execution-1", "claim-1", 1, 1, [0x41; 32], [0x61; 32]),
            CheckpointLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .unwrap(),
    );
    let original = checkpoint("original-frontier", "2026-10-02T10:00:00Z", 2);
    owner.save(&original).await.unwrap();
    let capture = CaptureAppend {
        inner: owner.clone(),
        candidate: Mutex::new(None),
    };
    prepare(&capture, "review").await;
    let first = capture.candidate.lock().await.take().unwrap();
    prepare(&capture, "publish").await;
    let stale = capture.candidate.lock().await.take().unwrap();
    owner.save(&first).await.unwrap();
    assert!(matches!(
        owner.save(&stale).await,
        Err(GraphError::CheckpointError(message)) if message.starts_with("checkpoint.conflict:")
    ));
    prepare(&capture, "publish").await;
    let mut changed = capture.candidate.lock().await.take().unwrap();
    changed.state.insert("counter".to_owned(), json!(999));
    assert!(matches!(
        owner.save(&changed).await,
        Err(GraphError::CheckpointError(message)) if message.starts_with("checkpoint.conflict:")
    ));
    assert_eq!(
        owner.load("thread-1").await.unwrap().unwrap().checkpoint_id,
        first.checkpoint_id
    );
    assert!(
        owner
            .load_by_id(&stale.checkpoint_id)
            .await
            .unwrap()
            .is_none()
    );
    assert!(
        owner
            .load_by_id(&changed.checkpoint_id)
            .await
            .unwrap()
            .is_none()
    );
}
