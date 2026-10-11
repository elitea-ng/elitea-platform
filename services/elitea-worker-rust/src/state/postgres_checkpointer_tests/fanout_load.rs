//! Fan-out preparation, parent-write budgets and load on real `PostgreSQL`.
//!
//! Budgets (Point 5 C2): at most 2 parent rows per activation visit, 2
//! transactions to prepare 16 children, per-child overhead p99 <= 30 ms, zero
//! pool-acquire timeouts at 32 executions x 8 children, restore of 64 children
//! p95 <= 500 ms. Every test skips unless `ELITEA_TEST_DATABASE_URL` is set.
use std::collections::BTreeSet;
use std::time::Instant;

use super::*;
use crate::agents::graph::{
    AdkParallelBranchRuntime, DurableParallelNode, MAX_PARENT_ROWS_PER_VISIT,
    PARALLEL_INTERRUPT_SCHEMA, PARALLEL_RESUME_STATE_KEY, ParallelActivation, ParallelBlocked,
    ParallelBranchDefinition, ParallelBranchExecution, ParallelBranchGraphFactory,
    ParallelBranchOutcome, ParallelBranchPause, ParallelBranchRuntime, ParallelBranchTerminal,
    ParallelCheckpointAppender, ParallelChildCheckpointerFactory, ParallelChildRequest,
    ParallelDecision, ParallelNodeDefinition, ParallelOccurrenceCheckpointer, ParallelPauseCard,
    PreparedParallelActivation, PreparedParallelBranch, fanout_budget,
};
use adk_rust::graph::{
    CompiledGraph, FunctionNode, GraphError, Node, NodeContext, interrupt::Interrupt,
};
use async_trait::async_trait;
use serde_json::Value;
use tokio::sync::Mutex as AsyncMutex;

const DEFINITION: [u8; 32] = [0x52; 32];
const RESUME_KEY: &str = "__elitea_hitl_resume_v1";
const PER_CHILD_P99_BUDGET: Duration = fanout_budget::CHILD_OVERHEAD_P99;
const RESTORE_64_P95_BUDGET: Duration = fanout_budget::RESTORE_MAX_CHILDREN_P95;
const PREPARE_TRANSACTIONS_BUDGET: u64 = fanout_budget::MAX_PREPARE_TRANSACTIONS;
const TRANSACTIONS_PER_CHILD_BUDGET: u64 = fanout_budget::MAX_CHILD_TRANSACTIONS;
/// Parent transactions per completed first visit: ADK's two empty start
/// probes of a new root, freeze read and append, batched preparation (2), join
/// head probe and append.
const PARENT_TRANSACTIONS_PER_VISIT: u64 = 8;
/// Wall-clock budgets need a quiet machine; set this to `enforce` for the
/// recorded run. Deterministic budgets are always enforced.
const LATENCY_BUDGETS_ENV: &str = "ELITEA_FANOUT_LATENCY_BUDGETS";

fn authority(thread: &str, execution: &str, attempt: u64) -> CheckpointWriterAuthority {
    CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        1,
        APPLICATION_CAPABILITY_ID,
        DEFINITION,
        thread.to_owned(),
        execution.to_owned(),
        1,
        format!("claim-{execution}-{attempt}"),
        attempt,
        attempt,
        1_700_000_000_000_000 + i64::try_from(attempt).expect("attempt") * 1_000_000,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        [0x61; 32],
    )
    .expect("valid fan-out writer")
}

async fn root(
    pool: &PgPool,
    thread: &str,
    execution: &str,
    attempt: u64,
) -> Arc<PostgresCheckpointer> {
    Arc::new(
        PostgresCheckpointer::activate(
            pool.clone(),
            authority(thread, execution, attempt),
            CheckpointLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .expect("activate the root writer"),
    )
}

async fn parent_rows(pool: &PgPool, thread: &str) -> usize {
    let rows = sqlx::query_scalar::<_, i64>(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id = $1",
    )
    .bind(thread)
    .fetch_one(pool)
    .await
    .expect("count parent rows");
    usize::try_from(rows).expect("row count")
}

/// Instant children: a single owned node per branch. `pausing` names pause
/// until a decision arrives. Runs are counted per branch node.
struct InstantBranches {
    runs: Arc<std::sync::Mutex<HashMap<String, usize>>>,
    pausing: BTreeSet<String>,
}

impl InstantBranches {
    fn new(pausing: &[&str]) -> Arc<Self> {
        Arc::new(Self {
            runs: Arc::default(),
            pausing: pausing.iter().map(|name| (*name).to_owned()).collect(),
        })
    }

    fn runs(&self, name: &str) -> usize {
        self.runs
            .lock()
            .expect("run counter")
            .get(name)
            .copied()
            .unwrap_or_default()
    }
}

#[async_trait]
impl ParallelBranchGraphFactory for InstantBranches {
    fn validate_branch(&self, _branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        Ok(())
    }

    fn owned_definition_digest(
        &self,
        branch: &ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError> {
        let mut digest = [0; 32];
        digest.copy_from_slice(
            ring::digest::digest(&ring::digest::SHA256, branch.node().as_bytes()).as_ref(),
        );
        Ok(digest)
    }

    fn compile_branch(
        &self,
        branch: &ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let name = branch.node().to_owned();
        let pauses = self.pausing.contains(&name);
        let runs = Arc::clone(&self.runs);
        let node_name = name.clone();
        let work = FunctionNode::new(&name, move |context| {
            let runs = Arc::clone(&runs);
            let name = node_name.clone();
            async move {
                *runs
                    .lock()
                    .map_err(|_| GraphError::Other("run counter".to_owned()))?
                    .entry(name.clone())
                    .or_default() += 1;
                if pauses && !context.state.contains_key(RESUME_KEY) {
                    return Ok(NodeOutput::interrupt_with_data(
                        "Child paused.",
                        json!({"interrupt_id": format!("card-{name}"), "tool_call_id": format!("call-{name}")}),
                    ));
                }
                Ok(NodeOutput::new().with_update("branch_result", json!({"output": name})))
            }
        });
        StateGraph::with_channels(&["branch_input", "branch_result", RESUME_KEY])
            .add_node(execution.wrap_node(work))
            .add_edge(START, &name)
            .add_edge(&name, END)
            .compile()
            .map(|graph| {
                graph
                    .with_checkpointer_arc(execution.checkpointer())
                    .with_strict_channels()
            })
    }

    fn project_input(
        &self,
        _branch: &ParallelBranchDefinition,
        parent: &State,
    ) -> Result<State, GraphError> {
        Ok(HashMap::from([(
            "branch_input".to_owned(),
            parent.get("input").cloned().unwrap_or(Value::Null),
        )]))
    }

    fn project_result(
        &self,
        _branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError> {
        child
            .get("branch_result")
            .and_then(Value::as_object)
            .cloned()
            .map(ParallelBranchTerminal::Completed)
            .ok_or_else(|| GraphError::Other("result missing".to_owned()))
    }

    fn pause_cards(
        &self,
        _branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        let Interrupt::Dynamic {
            data: Some(data), ..
        } = &pause.interrupt
        else {
            return Err(GraphError::Other("pause missing".to_owned()));
        };
        Ok(vec![serde_json::from_value(data.clone()).map_err(
            |_| GraphError::Other("pause malformed".to_owned()),
        )?])
    }

    async fn resume_input(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        Ok(HashMap::from([(
            RESUME_KEY.to_owned(),
            json!({"action": decisions.first().map(|decision| decision.action.clone())}),
        )]))
    }
}

/// Times each child invocation; everything else delegates unchanged.
struct TimedRuntime {
    inner: Arc<AdkParallelBranchRuntime>,
    child_durations: AsyncMutex<Vec<Duration>>,
}

#[async_trait]
impl ParallelBranchRuntime for TimedRuntime {
    fn validate(&self, definition: &ParallelNodeDefinition) -> Result<(), GraphError> {
        self.inner.validate(definition)
    }

    async fn prepare(
        &self,
        activation: &mut ParallelActivation,
        definition: &ParallelNodeDefinition,
        context: &NodeContext,
    ) -> Result<PreparedParallelActivation, GraphError> {
        self.inner.prepare(activation, definition, context).await
    }

    async fn record_pause(
        &self,
        activation: &ParallelActivation,
        cards: Vec<(usize, ParallelPauseCard)>,
    ) -> Result<(), GraphError> {
        self.inner.record_pause(activation, cards).await
    }

    async fn record_blocked(
        &self,
        activation: &ParallelActivation,
        blocked: ParallelBlocked,
    ) -> Result<(), GraphError> {
        self.inner.record_blocked(activation, blocked).await
    }

    async fn invoke(
        &self,
        activation: &ParallelActivation,
        branch: PreparedParallelBranch,
        context: &NodeContext,
    ) -> ParallelBranchOutcome {
        let started = Instant::now();
        let outcome = self.inner.invoke(activation, branch, context).await;
        self.child_durations.lock().await.push(started.elapsed());
        outcome
    }
}

fn runtime(
    checkpointer: &Arc<PostgresCheckpointer>,
    branches: &Arc<InstantBranches>,
) -> Arc<AdkParallelBranchRuntime> {
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpointer.clone();
    Arc::new(AdkParallelBranchRuntime::new(
        checkpointer.clone(),
        branches.clone(),
        Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
    ))
}

fn definition(branches: usize, max_concurrency: usize) -> ParallelNodeDefinition {
    use std::fmt::Write as _;
    let mut yaml = String::from("id: gather\ntype: parallel\nbranches:\n");
    for index in 0..branches {
        let _ = write!(yaml, "  - id: b{index}\n    node: n{index}\n");
    }
    let _ = write!(
        yaml,
        "max_concurrency: {max_concurrency}\nwait: all\nerror_policy: fail_after_drain\noutput: [gathered]\ntransition: END\n"
    );
    ParallelNodeDefinition::from_yaml(&yaml).expect("valid parallel fixture")
}

/// The production parent composition: the ADK parent and the branch runtime
/// share one occurrence wrapper (`parallel_compiler.rs` binds the same Arc).
fn parent_graph(
    runtime: &Arc<AdkParallelBranchRuntime>,
    definition: ParallelNodeDefinition,
    node_runtime: Arc<dyn ParallelBranchRuntime>,
) -> CompiledGraph {
    StateGraph::with_channels(&["input", "context", "gathered", PARALLEL_RESUME_STATE_KEY])
        .add_node(DurableParallelNode::new(definition, node_runtime))
        .add_edge(START, "gather")
        .add_edge("gather", END)
        .compile()
        .expect("compile parent graph")
        .with_checkpointer_arc(runtime.occurrence_checkpointer())
        .with_strict_channels()
}

/// One runtime used as both the node runtime and the parent wrapper owner.
fn graph(
    runtime: &Arc<AdkParallelBranchRuntime>,
    definition: ParallelNodeDefinition,
) -> CompiledGraph {
    parent_graph(runtime, definition, runtime.clone())
}

fn input_state() -> State {
    HashMap::from([("input".to_owned(), json!("original"))])
}

fn approve_all(pause: &Value) -> State {
    let decisions = pause["cards"]
        .as_array()
        .expect("aggregate cards")
        .iter()
        .map(|card| {
            json!({"interrupt_id": card["interrupt_id"], "tool_call_id": card["tool_call_id"], "action": "approve", "value": ""})
        })
        .collect::<Vec<_>>();
    HashMap::from([
        ("input".to_owned(), json!("original")),
        (
            PARALLEL_RESUME_STATE_KEY.to_owned(),
            json!({"schema": PARALLEL_INTERRUPT_SCHEMA, "parallel_activation": pause["parallel_activation"], "decisions": decisions}),
        ),
    ])
}

fn activation(thread: &str, definition: &ParallelNodeDefinition) -> ParallelActivation {
    ParallelActivation {
        root_thread_id: thread.to_owned(),
        node_id: definition.id().to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    }
}

fn percentile(samples: &mut [Duration], percentile: usize) -> Duration {
    samples.sort();
    let rank = (samples.len() * percentile).div_ceil(100).max(1);
    samples[rank - 1]
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One claim lifecycle: prepare, receipt read, takeover, fencing.
async fn batched_preparation_costs_two_transactions_and_refuses_a_superseded_claim() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL fan-out preparation test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let thread = "prepare-root";
    let first = root(&database.pool, thread, "execution-1", 1).await;
    let definition = definition(16, 8);
    let activation = activation(thread, &definition);
    let origin = first.child_origin(&activation).expect("child origin");
    let digests = (0..16_u8).map(|index| [index; 32]).collect::<Vec<_>>();
    let requests = definition
        .branches()
        .iter()
        .zip(&digests)
        .enumerate()
        .map(|(ordinal, (branch, input_digest))| ParallelChildRequest {
            branch,
            ordinal,
            input_digest,
        })
        .collect::<Vec<_>>();

    let before = (
        first.io_counters().transactions(),
        first.io_counters().round_trips(),
    );
    let prepared = first
        .prepare_children(&activation, &requests, &origin)
        .await
        .expect("prepare sixteen children");
    let transactions = first.io_counters().transactions() - before.0;
    let round_trips = first.io_counters().round_trips() - before.1;
    eprintln!("C2 prepare 16 children: transactions={transactions} round_trips={round_trips}");
    assert_eq!(transactions, PREPARE_TRANSACTIONS_BUDGET);
    assert!(round_trips <= 8, "{round_trips} round trips");
    assert_eq!(prepared.len(), 16);
    assert!(prepared.iter().all(|child| child.latest.is_none()));
    let threads = prepared
        .iter()
        .map(|child| child.child.thread_id.clone())
        .collect::<BTreeSet<_>>();
    assert_eq!(threads.len(), 16);
    for (ordinal, child) in prepared.iter().enumerate() {
        assert_eq!(
            child.child.thread_id,
            first
                .branch_thread_id(
                    &activation,
                    &definition.branches()[ordinal],
                    ordinal,
                    &digests[ordinal],
                    &origin
                )
                .expect("derived thread")
        );
    }

    // A child writes its receipt; the batched read returns exactly that row.
    let child = &prepared[3];
    let saved = Checkpoint::new(&child.child.thread_id, input_state(), 1, Vec::new());
    child
        .child
        .checkpointer
        .save(&saved)
        .await
        .expect("child save");
    let again = first
        .prepare_children(&activation, &requests, &origin)
        .await
        .expect("re-prepare is idempotent for the same claim");
    assert_eq!(
        again[3]
            .latest
            .as_ref()
            .map(|latest| latest.checkpoint_id.clone()),
        Some(saved.checkpoint_id.clone())
    );
    assert!(
        again
            .iter()
            .enumerate()
            .all(|(index, child)| index == 3 || child.latest.is_none())
    );

    // A later claim takes over the run: the earlier claim's batch is refused,
    // and its already-activated child writers can no longer write.
    let second = root(&database.pool, thread, "execution-2", 2).await;
    let refused = first
        .prepare_children(&activation, &requests, &origin)
        .await
        .err()
        .expect("superseded claim is refused");
    assert!(
        refused
            .to_string()
            .contains("checkpoint.writer_not_current"),
        "{refused}"
    );
    let second_origin = second.child_origin(&activation).expect("origin");
    let taken = second
        .prepare_children(&activation, &requests, &origin)
        .await
        .expect("the current claim activates the same frozen children");
    assert_ne!(second_origin, origin);
    let stale = again[5]
        .child
        .checkpointer
        .save(&Checkpoint::new(
            &again[5].child.thread_id,
            State::new(),
            1,
            Vec::new(),
        ))
        .await
        .expect_err("a superseded child writer is fenced");
    assert!(
        stale.to_string().contains("checkpoint.writer_not_current"),
        "{stale}"
    );
    assert_eq!(
        taken[3]
            .latest
            .as_ref()
            .map(|latest| latest.checkpoint_id.clone()),
        Some(saved.checkpoint_id)
    );
}

/// The production factory: each branch is a family of its root thread plus its
/// admitted application threads. All families activate in one transaction and
/// only the branch roots' receipts are read.
#[tokio::test]
#[allow(clippy::too_many_lines)] // One family lifecycle: prepare, fencing, receipt read.
async fn application_families_prepare_in_two_transactions_and_keep_their_own_threads() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL application family test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let thread = "family-root";
    let root = PostgresCheckpointer::activate(
        database.pool.clone(),
        authority(thread, "family-1", 1),
        CheckpointLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("activate the root writer");
    let family = root
        .with_application_paths(&["n0".to_owned(), "n0/inner".to_owned(), "n1".to_owned()])
        .await
        .expect("admit the application family");
    let counters = family.for_thread(thread).expect("root").io_counters();
    let definition = definition(2, 2);
    let activation = activation(thread, &definition);
    let origin = family.child_origin(&activation).expect("origin");
    let digests = [[1_u8; 32], [2_u8; 32]];
    let requests = definition
        .branches()
        .iter()
        .zip(&digests)
        .enumerate()
        .map(|(ordinal, (branch, input_digest))| ParallelChildRequest {
            branch,
            ordinal,
            input_digest,
        })
        .collect::<Vec<_>>();
    let before = counters.transactions();
    let prepared = family
        .prepare_children(&activation, &requests, &origin)
        .await
        .expect("prepare both families");
    assert_eq!(
        counters.transactions() - before,
        PREPARE_TRANSACTIONS_BUDGET
    );
    let (first, second) = (&prepared[0].child, &prepared[1].child);
    assert_eq!(
        first.admitted_threads,
        BTreeSet::from([
            first.thread_id.clone(),
            format!("{}/n0", first.thread_id),
            format!("{}/n0/inner", first.thread_id),
        ])
    );
    assert_eq!(
        second.admitted_threads,
        BTreeSet::from([second.thread_id.clone(), format!("{}/n1", second.thread_id)])
    );
    for (ordinal, child) in [first, second].into_iter().enumerate() {
        assert_eq!(
            child.thread_id,
            family
                .branch_thread_id(
                    &activation,
                    &definition.branches()[ordinal],
                    ordinal,
                    &digests[ordinal],
                    &origin,
                )
                .expect("derived branch thread")
        );
    }

    // A family writes only its own threads.
    let nested = format!("{}/n0/inner", first.thread_id);
    first
        .checkpointer
        .save(&Checkpoint::new(&nested, State::new(), 1, Vec::new()))
        .await
        .expect("nested thread of the first family");
    let foreign = format!("{}/n0", second.thread_id);
    assert!(
        second
            .checkpointer
            .save(&Checkpoint::new(&foreign, State::new(), 1, Vec::new()))
            .await
            .is_err()
    );
    // The receipt of the second branch root lands on the second branch only;
    // the first family's nested row is not a branch receipt.
    let receipt = Checkpoint::new(&second.thread_id, input_state(), 1, Vec::new());
    second
        .checkpointer
        .save(&receipt)
        .await
        .expect("branch receipt");
    let again = family
        .prepare_children(&activation, &requests, &origin)
        .await
        .expect("re-prepare");
    assert!(again[0].latest.is_none());
    assert_eq!(
        again[1]
            .latest
            .as_ref()
            .map(|latest| latest.checkpoint_id.clone()),
        Some(receipt.checkpoint_id)
    );
}

#[tokio::test]
async fn concurrent_parent_appends_against_one_head_admit_exactly_one() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL parent append race test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let thread = "append-root";
    let writer = root(&database.pool, thread, "execution-1", 1).await;
    let base = Checkpoint::new(thread, input_state(), 0, vec!["gather".to_owned()]);
    writer.save(&base).await.expect("base row");
    let probe = writer
        .probe_parent(thread, "candidate-not-stored")
        .await
        .expect("head probe");
    assert!(!probe.candidate_exists);
    let head = probe.latest.expect("latest head");
    assert_eq!(head.checkpoint_id, base.checkpoint_id);
    assert!(
        head.snapshot.is_none(),
        "the PostgreSQL head carries no state"
    );

    for _ in 0..8 {
        let latest = writer
            .probe_parent(thread, "candidate-not-stored")
            .await
            .expect("head probe")
            .latest
            .expect("latest head");
        let left = Checkpoint::new(thread, input_state(), latest.step + 1, Vec::new());
        let right = Checkpoint::new(thread, input_state(), latest.step + 1, Vec::new());
        let (left, right) = tokio::join!(
            writer.append_after_head(Some(&latest), &left),
            writer.append_after_head(Some(&latest), &right),
        );
        let outcomes = [left, right];
        assert_eq!(outcomes.iter().filter(|outcome| outcome.is_ok()).count(), 1);
        let refused = outcomes
            .iter()
            .find_map(|outcome| outcome.as_ref().err())
            .expect("one append refused");
        assert!(
            refused.to_string().contains("checkpoint.conflict"),
            "{refused}"
        );
        // A stale head is refused after the winner committed.
        let stale = writer
            .append_after_head(
                Some(&latest),
                &Checkpoint::new(thread, input_state(), latest.step + 2, Vec::new()),
            )
            .await
            .expect_err("stale head refused");
        assert!(stale.to_string().contains("checkpoint.conflict"), "{stale}");
    }
    // Exact immutable replay of the base row stays a no-op and is not latest.
    writer
        .append_after(None, &base)
        .await
        .expect("exact replay");
    let latest = writer.load(thread).await.expect("load").expect("latest");
    assert_ne!(latest.checkpoint_id, base.checkpoint_id);
}

/// Completion, pause, process replacement and resume each stay within the
/// per-visit parent-row budget, and no completed child runs twice.
#[tokio::test]
#[allow(clippy::too_many_lines)] // One parent lifecycle across crash windows and claims.
async fn every_visit_appends_at_most_two_parent_rows_across_crash_windows() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL parent-row budget test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let pool = &database.pool;

    // Completion visit: freeze + join.
    let thread = "budget-join";
    let writer = root(pool, thread, "join-1", 1).await;
    let branches = InstantBranches::new(&[]);
    graph(&runtime(&writer, &branches), definition(4, 4))
        .invoke(input_state(), ExecutionConfig::new(thread))
        .await
        .expect("join");
    assert_eq!(parent_rows(pool, thread).await, MAX_PARENT_ROWS_PER_VISIT);

    // Crash window: children completed, join row never written. A replacement
    // claim restores every receipt and writes only the join row.
    let thread = "budget-after-children";
    let first = root(pool, thread, "children-1", 1).await;
    let branches = InstantBranches::new(&[]);
    let output = DurableParallelNode::new(definition(4, 2), runtime(&first, &branches))
        .execute(&NodeContext::new(
            input_state(),
            ExecutionConfig::new(thread),
            0,
        ))
        .await
        .expect("children complete before the crash");
    assert!(output.interrupt.is_none());
    assert_eq!(
        parent_rows(pool, thread).await,
        1,
        "only the freeze row exists"
    );
    drop(first);
    let second = root(pool, thread, "children-2", 2).await;
    let result = graph(&runtime(&second, &branches), definition(4, 2))
        .invoke(input_state(), ExecutionConfig::new(thread))
        .await
        .expect("replacement joins from receipts");
    assert_eq!(result["gathered"].as_array().map(Vec::len), Some(4));
    assert_eq!(parent_rows(pool, thread).await, MAX_PARENT_ROWS_PER_VISIT);
    for index in 0..4 {
        assert_eq!(
            branches.runs(&format!("n{index}")),
            1,
            "a completed child ran again"
        );
    }

    // Crash window: after freeze and batched activation, before any child ran.
    let thread = "budget-after-activation";
    let first = root(pool, thread, "activation-1", 1).await;
    let branches = InstantBranches::new(&[]);
    let definition_four = definition(4, 4);
    let mut prepared_activation = activation(thread, &definition_four);
    let first_runtime = runtime(&first, &branches);
    let PreparedParallelActivation::Ready(_dropped) = first_runtime
        .prepare(
            &mut prepared_activation,
            &definition_four,
            &NodeContext::new(input_state(), ExecutionConfig::new(thread), 0),
        )
        .await
        .expect("freeze and prepare")
    else {
        panic!("unexpected blocked occurrence");
    };
    drop(first_runtime);
    let second = root(pool, thread, "activation-2", 2).await;
    graph(&runtime(&second, &branches), definition_four)
        .invoke(input_state(), ExecutionConfig::new(thread))
        .await
        .expect("replacement runs every child once");
    assert_eq!(parent_rows(pool, thread).await, MAX_PARENT_ROWS_PER_VISIT);
    for index in 0..4 {
        assert_eq!(branches.runs(&format!("n{index}")), 1);
    }

    // Pause visit: freeze + one pause row that carries the cards. The pause
    // row is written by the ADK save; record_pause appends nothing.
    let thread = "budget-pause";
    let first = root(pool, thread, "pause-1", 1).await;
    let branches = InstantBranches::new(&["n1", "n2"]);
    let pausing = graph(&runtime(&first, &branches), definition(3, 3));
    let GraphError::Interrupted(interrupted) = pausing
        .invoke(input_state(), ExecutionConfig::new(thread))
        .await
        .expect_err("two branches pause")
    else {
        panic!("expected the aggregate pause");
    };
    assert_eq!(parent_rows(pool, thread).await, MAX_PARENT_ROWS_PER_VISIT);
    let paused = first.load(thread).await.expect("load").expect("pause row");
    assert_eq!(paused.checkpoint_id, interrupted.checkpoint_id);
    assert_eq!(
        paused.metadata["elitea.graph.parallel.occurrence.v2"]["cards"]
            .as_array()
            .map(Vec::len),
        Some(2),
        "the pause row carries both cards"
    );
    let Interrupt::Dynamic {
        data: Some(pause), ..
    } = interrupted.interrupt
    else {
        panic!("expected aggregate pause data");
    };

    // Resume visit under a new execution (process replacement): accepted
    // decision set + join, still two rows; completed children do not re-run.
    let resumed_writer = root(pool, thread, "pause-2", 2).await;
    let result = graph(&runtime(&resumed_writer, &branches), definition(3, 3))
        .invoke(
            approve_all(&pause),
            ExecutionConfig::new(thread).with_resume_from(&interrupted.checkpoint_id),
        )
        .await
        .expect("resume joins");
    assert_eq!(result["gathered"].as_array().map(Vec::len), Some(3));
    assert_eq!(
        parent_rows(pool, thread).await,
        2 * MAX_PARENT_ROWS_PER_VISIT
    );
    assert_eq!(branches.runs("n0"), 1);
    assert_eq!(branches.runs("n1"), 2);
    assert_eq!(branches.runs("n2"), 2);

    // Crash window: the pause was computed but its row was never saved. The
    // replacement derives the same cards from the child receipts.
    let thread = "budget-pause-lost";
    let first = root(pool, thread, "lost-1", 1).await;
    let branches = InstantBranches::new(&["n0"]);
    let lost = DurableParallelNode::new(definition(2, 2), runtime(&first, &branches))
        .execute(&NodeContext::new(
            input_state(),
            ExecutionConfig::new(thread),
            0,
        ))
        .await
        .expect("pause computed");
    assert!(lost.interrupt.is_some());
    assert_eq!(
        parent_rows(pool, thread).await,
        1,
        "the staged cards appended nothing"
    );
    drop(first);
    let second = root(pool, thread, "lost-2", 2).await;
    let GraphError::Interrupted(again) = graph(&runtime(&second, &branches), definition(2, 2))
        .invoke(input_state(), ExecutionConfig::new(thread))
        .await
        .expect_err("the same pause is published")
    else {
        panic!("expected the aggregate pause");
    };
    let Interrupt::Dynamic {
        data: Some(again), ..
    } = again.interrupt
    else {
        panic!("expected aggregate pause data");
    };
    assert_eq!(again["cards"][0]["interrupt_id"], "card-n0");
    assert_eq!(parent_rows(pool, thread).await, MAX_PARENT_ROWS_PER_VISIT);
    assert_eq!((branches.runs("n0"), branches.runs("n1")), (1, 1));
}

/// C2d load test against the budgets. Records the measured numbers.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[allow(clippy::too_many_lines)] // Three measured scenarios report one budget table.
async fn fanout_load_meets_the_postgres_budgets() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL fan-out load test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    // The production agentstate pool: 32 connections, 5 s acquire timeout.
    let pool = PgPoolOptions::new()
        .max_connections(32)
        .acquire_timeout(Duration::from_secs(5))
        .connect_with(
            database
                .admin_options
                .clone()
                .database(&database.database_name),
        )
        .await
        .expect("production-sized pool");
    let context_bytes = "x".repeat(512 * 1024);

    // Warm the pool: connection set-up is not per-child overhead.
    let mut warm = Vec::with_capacity(32);
    for _ in 0..32 {
        warm.push(pool.acquire().await.expect("warm connection"));
    }
    drop(warm);

    // 1. Per-child overhead: sequential 8-child activations, instant children.
    let mut child_samples = Vec::new();
    let mut activation_samples = Vec::new();
    let mut activation_transactions = 0;
    let mut activation_round_trips = 0;
    for execution in 0..12 {
        let thread = format!("overhead-{execution}");
        let writer = root(&pool, &thread, &format!("overhead-{execution}"), 1).await;
        let branches = InstantBranches::new(&[]);
        let inner = runtime(&writer, &branches);
        let timed = Arc::new(TimedRuntime {
            inner: inner.clone(),
            child_durations: AsyncMutex::new(Vec::new()),
        });
        let mut state = input_state();
        state.insert("context".to_owned(), json!(context_bytes));
        let transactions = writer.io_counters().transactions();
        let round_trips = writer.io_counters().round_trips();
        let started = Instant::now();
        parent_graph(&inner, definition(8, 8), timed.clone())
            .invoke(state, ExecutionConfig::new(&thread))
            .await
            .expect("overhead activation");
        activation_samples.push(started.elapsed());
        // The root's counters include every child it activated.
        activation_transactions = writer.io_counters().transactions() - transactions;
        activation_round_trips = writer.io_counters().round_trips() - round_trips;
        assert_eq!(
            activation_transactions,
            PARENT_TRANSACTIONS_PER_VISIT + 8 * TRANSACTIONS_PER_CHILD_BUDGET,
            "transactions for one 8-child activation"
        );
        assert!(parent_rows(&pool, &thread).await <= MAX_PARENT_ROWS_PER_VISIT);
        child_samples.extend(timed.child_durations.lock().await.iter().copied());
    }
    let child_p50 = percentile(&mut child_samples, 50);
    let child_p99 = percentile(&mut child_samples, 99);
    let activation_p50 = percentile(&mut activation_samples, 50);

    // 2. 32 executions x 8 children at once on the 32-connection pool.
    let started = Instant::now();
    let runs = (0..32)
        .map(|execution| {
            let pool = pool.clone();
            let context_bytes = context_bytes.clone();
            tokio::spawn(async move {
                let thread = format!("load-{execution}");
                let writer = root(&pool, &thread, &format!("load-{execution}"), 1).await;
                let branches = InstantBranches::new(&[]);
                let mut state = input_state();
                state.insert("context".to_owned(), json!(context_bytes));
                let result = graph(&runtime(&writer, &branches), definition(8, 8))
                    .invoke(state, ExecutionConfig::new(&thread))
                    .await;
                (result.map(|_| ()), parent_rows(&pool, &thread).await)
            })
        })
        .collect::<Vec<_>>();
    let mut failures = Vec::new();
    let mut max_rows = 0;
    for run in runs {
        let (result, rows) = run.await.expect("load execution task");
        max_rows = max_rows.max(rows);
        if let Err(error) = result {
            failures.push(error.to_string());
        }
    }
    let load_wall = started.elapsed();

    // 3. Restore of 64 completed children: one batched activation + one read.
    let thread = "restore-root";
    let writer = root(&pool, thread, "restore-1", 1).await;
    let wide = definition(16, 8);
    let activation = activation(thread, &wide);
    let origin = writer.child_origin(&activation).expect("origin");
    let digests = (0..64_u8).map(|index| [index; 32]).collect::<Vec<_>>();
    let requests = digests
        .iter()
        .enumerate()
        .map(|(ordinal, input_digest)| ParallelChildRequest {
            branch: &wide.branches()[ordinal % 16],
            ordinal,
            input_digest,
        })
        .collect::<Vec<_>>();
    let children = writer
        .prepare_children(&activation, &requests, &origin)
        .await
        .expect("activate 64 children");
    for child in &children {
        let mut terminal = Checkpoint::new(
            &child.child.thread_id,
            HashMap::from([(
                "branch_result".to_owned(),
                json!({"output": "x".repeat(1024)}),
            )]),
            1,
            Vec::new(),
        );
        terminal.metadata.insert(
            "elitea.graph.parallel.branch-receipt.v1".to_owned(),
            json!({"status": "completed"}),
        );
        child
            .child
            .checkpointer
            .save(&terminal)
            .await
            .expect("child receipt");
    }
    let mut restore_samples = Vec::new();
    for _ in 0..20 {
        let started = Instant::now();
        let transactions = writer.io_counters().transactions();
        let restored = writer
            .prepare_children(&activation, &requests, &origin)
            .await
            .expect("restore 64 children");
        restore_samples.push(started.elapsed());
        assert_eq!(
            writer.io_counters().transactions() - transactions,
            PREPARE_TRANSACTIONS_BUDGET
        );
        assert!(restored.iter().all(|child| child.latest.is_some()));
    }
    let restore_p95 = percentile(&mut restore_samples, 95);

    eprintln!(
        "C2 fan-out load budgets:\n  one 8-child activation: transactions={activation_transactions} round_trips={activation_round_trips} (budget {} transactions)\n  per-child overhead p50={child_p50:?} p99={child_p99:?} (budget p99 <= {PER_CHILD_P99_BUDGET:?}, n={})\n  8-child activation p50={activation_p50:?} (parent context 512 KiB)\n  32 executions x 8 children: wall={load_wall:?} failures={} max parent rows={max_rows} (budget 0 failures, <= {MAX_PARENT_ROWS_PER_VISIT} rows)\n  restore 64 children p95={restore_p95:?} (budget <= {RESTORE_64_P95_BUDGET:?})",
        PARENT_TRANSACTIONS_PER_VISIT + 8 * TRANSACTIONS_PER_CHILD_BUDGET,
        child_samples.len(),
        failures.len(),
    );
    assert!(
        failures.is_empty(),
        "pool or storage failures: {failures:?}"
    );
    assert!(max_rows <= MAX_PARENT_ROWS_PER_VISIT);
    if env::var(LATENCY_BUDGETS_ENV).as_deref() == Ok("enforce") {
        assert!(
            child_p99 <= PER_CHILD_P99_BUDGET,
            "per-child p99 {child_p99:?}"
        );
        assert!(
            restore_p95 <= RESTORE_64_P95_BUDGET,
            "restore p95 {restore_p95:?}"
        );
    }
    pool.close().await;
}

const LOG_CHILD_ENV: &str = "ELITEA_FANOUT_LOG_CHILD";
const SENSITIVE_MARKER: &str = "fanout-sensitive-prompt-9c41";

/// Fan-out spans, lifecycle logs and checkpoint spans carry safe fields and
/// I/O counts only. A child process owns a global subscriber, so parallel
/// tests cannot change callsite interest while the logs are captured.
#[test]
fn fanout_and_checkpoint_spans_carry_counts_and_no_sensitive_values() {
    if env::var(TEST_DATABASE_URL).is_err() {
        eprintln!("skipping PostgreSQL fan-out log capture: set {TEST_DATABASE_URL}");
        return;
    }
    let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
        .args([
            "--exact",
            "state::postgres_checkpointer_tests::fanout_load::fanout_log_child",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(LOG_CHILD_ENV, "1")
        .output()
        .expect("log child process");
    let logged = String::from_utf8_lossy(&output.stdout);
    assert!(output.status.success(), "{logged}");
    assert!(logged.contains("CHILD-RAN"), "{logged}");
    for expected in [
        "graph.fanout.activation",
        "graph.fanout.child",
        "Fan-out started",
        "Fan-out child admitted",
        "Fan-out child completed",
        "Fan-out joined",
        "agent.checkpoint.persist",
        "operation=\"activate_children\"",
        "operation=\"load_children\"",
        "operation=\"parent_probe\"",
        "payload_bytes=",
        "round_trips=",
        "pool_wait_ms=",
    ] {
        assert!(logged.contains(expected), "missing {expected}: {logged}");
    }
    for forbidden in [
        SENSITIVE_MARKER,
        "p1:",
        "claim-",
        "fence",
        "SELECT",
        "tenant-1",
    ] {
        assert!(!logged.contains(forbidden), "leaked {forbidden}: {logged}");
    }
}

#[tokio::test]
async fn fanout_log_child() {
    if env::var(LOG_CHILD_ENV).is_err() {
        return;
    }
    tracing_subscriber::fmt()
        .with_ansi(false)
        .with_max_level(tracing::Level::TRACE)
        .with_span_events(tracing_subscriber::fmt::format::FmtSpan::CLOSE)
        .with_writer(std::io::stdout)
        .init();
    let database = IsolatedPostgres::create(&env::var(TEST_DATABASE_URL).expect("database")).await;
    install_test_schema(&database.pool).await;
    let thread = "log-root";
    let writer = root(&database.pool, thread, "log-1", 1).await;
    let branches = InstantBranches::new(&[]);
    let input = HashMap::from([("input".to_owned(), json!(SENSITIVE_MARKER))]);
    graph(&runtime(&writer, &branches), definition(2, 2))
        .invoke(input, ExecutionConfig::new(thread))
        .await
        .expect("logged activation");
    println!("CHILD-RAN");
}
