//! Fan-out V2 re-entry and crash seams on real `PostgreSQL` (design §7, §8;
//! T-2A 2a-2 tasks 1 and 5).
//!
//! The root pipeline graph is `pre -> gather -> post`. `gather` is the durable
//! Parallel node; one branch (`ask`) pauses with a dynamic interrupt, so
//! `gather` parks with `NodeOutput::interrupt_with_data` and ADK saves the
//! root frontier with `gather` still pending. Each scenario drops every
//! in-memory object (process replacement) and activates a new writer claim on
//! the same root thread. The checkpointer chain matches production
//! composition: `TurnCheckpointer -> ParallelOccurrenceCheckpointer ->
//! PostgresCheckpointer`.
//!
//! Decisions travel per interrupt through a claim-fenced ledger (the Main
//! decision API stand-in, [`TestLedger`]); nothing is delivered through graph
//! state. Proven per scenario: one final answer, one distinct card, no DECIDED
//! row left behind, completed children never invoked again, and the paused
//! child resumes from its own checkpoint (its `prep` node never runs again).
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Mutex as StdMutex;
use std::sync::atomic::AtomicBool;

use super::*;
use crate::agents::graph::fanout_test_ledger::{LedgerSeam, TestLedger};
use crate::agents::graph::fanout_turn::FanoutTurnAuthority;
use crate::agents::graph::turn_checkpointer::TurnCheckpointer;
use crate::agents::graph::{
    AdkParallelBranchRuntime, DurableParallelNode, FANOUT_PARKED_SCHEMA, ParallelBranchDefinition,
    ParallelBranchExecution, ParallelBranchGraphFactory, ParallelBranchPause,
    ParallelBranchTerminal, ParallelCheckpointAppender, ParallelCheckpointAuthority,
    ParallelChildCheckpoint, ParallelChildOrigin, ParallelChildRequest, ParallelDecision,
    ParallelOccurrenceCheckpointer, ParallelPauseCard, PreparedChildCheckpoint,
};
use adk_rust::graph::{CompiledGraph, FunctionNode, GraphError, interrupt::Interrupt};
use async_trait::async_trait;
use elitea_agent_runtime::graph::fanout::apply::CardIdentity;
use elitea_agent_runtime::graph::fanout::runner::{AckResult, FanoutEvents};
use serde_json::Value;
use tokio::sync::Notify;

const ROOT: &str = "thread-1";
const DEFINITION: [u8; 32] = [0x52; 32];
const RESUME_KEY: &str = "__elitea_hitl_resume_v1";
const BRANCHES: [&str; 3] = ["done", "ask", "late"];

/// Invocation counters. `prep` runs before a branch's owned node, so a paused
/// child restarted from scratch instead of restored shows up as `prep == 2`.
#[derive(Default)]
struct Runs {
    root: BTreeMap<&'static str, AtomicUsize>,
    prep: BTreeMap<String, AtomicUsize>,
    work: BTreeMap<String, AtomicUsize>,
}

impl Runs {
    fn new() -> Arc<Self> {
        let counters = || {
            BRANCHES
                .iter()
                .map(|name| ((*name).to_owned(), AtomicUsize::new(0)))
                .collect()
        };
        Arc::new(Self {
            root: ["pre", "post"]
                .into_iter()
                .map(|name| (name, AtomicUsize::new(0)))
                .collect(),
            prep: counters(),
            work: counters(),
        })
    }

    fn root(&self, name: &str) -> usize {
        self.root[name].load(Ordering::SeqCst)
    }

    fn prep(&self, name: &str) -> usize {
        self.prep[name].load(Ordering::SeqCst)
    }

    fn work(&self, name: &str) -> usize {
        self.work[name].load(Ordering::SeqCst)
    }

    /// `(pre, post, done, ask, ask prep, late)`.
    fn snapshot(&self) -> (usize, usize, usize, usize, usize, usize) {
        (
            self.root("pre"),
            self.root("post"),
            self.work("done"),
            self.work("ask"),
            self.prep("ask"),
            self.work("late"),
        )
    }
}

/// One crash point inside a process: the first matching call announces itself
/// and never returns, so the test can kill the process exactly there.
#[derive(Default)]
struct Seam {
    armed: AtomicBool,
    reached: Notify,
}

impl Seam {
    fn armed() -> Arc<Self> {
        let seam = Arc::new(Self::default());
        seam.armed.store(true, Ordering::SeqCst);
        seam
    }

    async fn stop_here(&self) {
        if self.armed.swap(false, Ordering::SeqCst) {
            self.reached.notify_one();
            std::future::pending::<()>().await;
        }
    }
}

/// Holds for one process: `late` blocks forever after it started; the
/// resumed `ask` blocks until released (a zombie claim mid-apply).
#[derive(Clone, Default)]
struct Holds {
    late_started: Option<Arc<Notify>>,
    resumed_ask: Option<(Arc<Notify>, Arc<Notify>)>,
}

struct Branches {
    runs: Arc<Runs>,
    holds: Holds,
}

#[async_trait]
impl ParallelBranchGraphFactory for Branches {
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        if self.runs.work.contains_key(branch.node()) {
            Ok(())
        } else {
            Err(GraphError::NodeNotFound(branch.node().to_owned()))
        }
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
        let prep_runs = Arc::clone(&self.runs);
        let prep_name = name.clone();
        let prep = FunctionNode::new("prep", move |_| {
            let runs = Arc::clone(&prep_runs);
            let name = prep_name.clone();
            async move {
                runs.prep[&name].fetch_add(1, Ordering::SeqCst);
                Ok(NodeOutput::new().with_update("prepared", json!(name)))
            }
        });
        let work_runs = Arc::clone(&self.runs);
        let work_name = name.clone();
        let late_hold = self.holds.late_started.clone().filter(|_| name == "late");
        let ask_hold = self.holds.resumed_ask.clone().filter(|_| name == "ask");
        let work = FunctionNode::new(&name, move |context| {
            let runs = Arc::clone(&work_runs);
            let name = work_name.clone();
            let late_hold = late_hold.clone();
            let ask_hold = ask_hold.clone();
            async move {
                runs.work[&name].fetch_add(1, Ordering::SeqCst);
                if let Some(started) = late_hold {
                    started.notify_one();
                    std::future::pending::<()>().await;
                }
                if name == "ask" && !context.state.contains_key(RESUME_KEY) {
                    return Ok(NodeOutput::interrupt_with_data(
                        "Child paused.",
                        json!({"interrupt_id": "card-ask", "tool_call_id": "call-ask"}),
                    ));
                }
                if let Some((entered, release)) = ask_hold {
                    entered.notify_one();
                    release.notified().await;
                }
                Ok(NodeOutput::new().with_update(
                    "branch_result",
                    json!({"output": name, "prepared": context.state.get("prepared")}),
                ))
            }
        });
        StateGraph::with_channels(&["branch_input", "prepared", "branch_result", RESUME_KEY])
            .add_node(prep)
            .add_node(execution.wrap_node(work))
            .add_edge(START, "prep")
            .add_edge("prep", &name)
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

/// A child store that stops forever right after its first terminal save
/// (the child's receipt is durable, nothing after it ran).
struct SeamChildStore {
    inner: Arc<dyn Checkpointer>,
    seam: Arc<Seam>,
}

#[async_trait]
impl Checkpointer for SeamChildStore {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let saved = self.inner.save(checkpoint).await?;
        if checkpoint.pending_nodes.is_empty() {
            self.seam.stop_here().await;
        }
        Ok(saved)
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
}

/// The production child factory with every child store wrapped by a seam.
struct SeamChildren {
    inner: Arc<PostgresCheckpointer>,
    seam: Arc<Seam>,
}

impl SeamChildren {
    fn wrap(&self, mut child: ParallelChildCheckpoint) -> ParallelChildCheckpoint {
        child.checkpointer = Arc::new(SeamChildStore {
            inner: child.checkpointer,
            seam: Arc::clone(&self.seam),
        });
        child
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for SeamChildren {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<ParallelChildOrigin, GraphError> {
        self.inner.child_origin(activation)
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        self.inner
            .branch_thread_id(activation, branch, ordinal, input_digest, origin)
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        Ok(self.wrap(
            self.inner
                .for_branch(activation, branch, ordinal, input_digest, origin)
                .await?,
        ))
    }

    async fn prepare_children(
        &self,
        activation: &ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        Ok(self
            .inner
            .prepare_children(activation, children, origin)
            .await?
            .into_iter()
            .map(|prepared| PreparedChildCheckpoint {
                child: self.wrap(prepared.child),
                latest: prepared.latest,
            })
            .collect())
    }
}

/// The root checkpointer, stopping forever before the save that leaves
/// `gather` (the join row is never written).
struct SeamRoot {
    inner: Arc<dyn Checkpointer>,
    seam: Arc<Seam>,
}

#[async_trait]
impl Checkpointer for SeamRoot {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        if checkpoint.pending_nodes == ["post"] {
            self.seam.stop_here().await;
        }
        self.inner.save(checkpoint).await
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
}

/// Every card frame any process raised (card ingestion is idempotent on the
/// key, so a re-raise on restore is the same card).
#[derive(Default)]
struct RaisedCards(StdMutex<Vec<String>>);

#[async_trait::async_trait]
impl FanoutEvents for RaisedCards {
    async fn card_opened(
        &self,
        _child: usize,
        card: &CardIdentity,
    ) -> Result<(), elitea_agent_runtime::graph::fanout::runner::FanoutRunError> {
        self.0
            .lock()
            .expect("raised cards")
            .push(card.interrupt_key.clone());
        Ok(())
    }
}

impl RaisedCards {
    fn distinct(&self) -> BTreeSet<String> {
        self.0
            .lock()
            .expect("raised cards")
            .iter()
            .cloned()
            .collect()
    }
}

fn definition(max_concurrency: usize) -> ParallelNodeDefinition {
    ParallelNodeDefinition::from_yaml(&format!(
        r"
id: gather
type: parallel
branches:
  - id: first
    node: done
  - id: second
    node: ask
  - id: third
    node: late
max_concurrency: {max_concurrency}
wait: all
error_policy: fail_after_drain
output: [gathered]
transition: post
        "
    ))
    .expect("valid three-branch parallel fixture")
}

/// One process: a writer claim, its checkpointer chain and the compiled root.
struct Process {
    postgres: Arc<dyn Checkpointer>,
    graph: CompiledGraph,
}

struct Turn<'a> {
    execution_id: &'a str,
    generation: u64,
    claim_attempt: u64,
    /// The pipeline start is an explicit continuation (`fanout_wake`).
    resume: bool,
}

/// What one process shares with the test.
struct Shared {
    runs: Arc<Runs>,
    ledger: Arc<TestLedger>,
    raised: Arc<RaisedCards>,
}

impl Shared {
    fn new() -> Self {
        Self {
            runs: Runs::new(),
            ledger: TestLedger::new(),
            raised: Arc::new(RaisedCards::default()),
        }
    }
}

/// The fenced writer as the one typed authority `FanoutTurnAuthority` wraps
/// (production holds the same writer behind `ApplicationCheckpointers`).
struct PostgresHolder(Arc<PostgresCheckpointer>);

#[async_trait]
impl Checkpointer for PostgresHolder {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.0.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.0.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.delete(thread).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for PostgresHolder {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        self.0.append_after(expected, candidate).await
    }
    async fn probe_parent(
        &self,
        thread_id: &str,
        candidate_id: &str,
    ) -> Result<crate::agents::graph::ParentSaveProbe, GraphError> {
        self.0.probe_parent(thread_id, candidate_id).await
    }
    async fn append_after_head(
        &self,
        expected: Option<&crate::agents::graph::ParentHead>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        self.0.append_after_head(expected, candidate).await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for PostgresHolder {
    fn child_origin(
        &self,
        activation: &crate::agents::graph::ParallelActivation,
    ) -> Result<ParallelChildOrigin, GraphError> {
        self.0.child_origin(activation)
    }
    fn branch_thread_id(
        &self,
        activation: &crate::agents::graph::ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        self.0
            .branch_thread_id(activation, branch, ordinal, input_digest, origin)
    }
    async fn for_branch(
        &self,
        activation: &crate::agents::graph::ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        self.0
            .for_branch(activation, branch, ordinal, input_digest, origin)
            .await
    }
    async fn prepare_children(
        &self,
        activation: &crate::agents::graph::ParallelActivation,
        children: &[ParallelChildRequest<'_>],
        origin: &ParallelChildOrigin,
    ) -> Result<Vec<PreparedChildCheckpoint>, GraphError> {
        self.0.prepare_children(activation, children, origin).await
    }
}

impl ParallelCheckpointAuthority for PostgresHolder {}

#[derive(Default)]
struct ProcessSeams {
    holds: Holds,
    child: Option<Arc<Seam>>,
    root: Option<Arc<Seam>>,
    /// Compose the parent as `bind_parallel_authority` does: occurrence ->
    /// `FanoutTurnAuthority` -> the production checkpoint holder.
    turn_authority: bool,
}

#[allow(clippy::too_many_lines)] // One process owns its writer, chain, runtime and root graph.
async fn start_process(
    database: &IsolatedPostgres,
    turn: &Turn<'_>,
    shared: &Shared,
    max_concurrency: usize,
    seams: ProcessSeams,
) -> Process {
    let postgres = Arc::new(
        PostgresCheckpointer::activate(
            database.pool.clone(),
            CheckpointWriterAuthority::new(
                "tenant-1".to_owned(),
                1,
                1,
                APPLICATION_CAPABILITY_ID,
                DEFINITION,
                ROOT.to_owned(),
                turn.execution_id.to_owned(),
                turn.generation,
                format!("claim-{}-{}", turn.execution_id, turn.claim_attempt),
                turn.claim_attempt,
                turn.claim_attempt,
                1_700_000_000_000_000
                    + i64::try_from(turn.claim_attempt).expect("claim attempt") * 1_000_000,
                "workload-1".to_owned(),
                "producer-1".to_owned(),
                [0x62; 32],
            )
            .expect("valid checkpoint writer"),
            CheckpointLimits::default(),
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .expect("activate the root checkpoint writer"),
    );
    let (postgres, appender, children): (
        Arc<dyn Checkpointer>,
        Arc<dyn ParallelCheckpointAppender>,
        Arc<dyn ParallelChildCheckpointerFactory>,
    ) = if seams.turn_authority {
        assert!(seams.child.is_none() && seams.root.is_none());
        let holder: Arc<dyn ParallelCheckpointAuthority> = Arc::new(PostgresHolder(postgres));
        let authority = Arc::new(
            FanoutTurnAuthority::new(
                Arc::clone(&holder),
                ROOT,
                turn.execution_id,
                turn.generation,
                turn.resume,
            )
            .expect("valid turn authority"),
        );
        (holder, authority.clone(), authority)
    } else {
        let children: Arc<dyn ParallelChildCheckpointerFactory> = match seams.child {
            Some(seam) => Arc::new(SeamChildren {
                inner: postgres.clone(),
                seam,
            }),
            None => postgres.clone(),
        };
        (postgres.clone(), postgres, children)
    };
    let occurrence = Arc::new(ParallelOccurrenceCheckpointer::new(appender));
    let runtime = Arc::new(AdkParallelBranchRuntime::new(
        children,
        Arc::new(Branches {
            runs: Arc::clone(&shared.runs),
            holds: seams.holds,
        }),
        Arc::clone(&occurrence),
    ));
    let mut root: Arc<dyn Checkpointer> = if seams.turn_authority {
        occurrence.clone()
    } else {
        Arc::new(TurnCheckpointer::new(
            occurrence.clone(),
            turn.execution_id,
            turn.generation,
            turn.resume,
        ))
    };
    if let Some(seam) = seams.root {
        root = Arc::new(SeamRoot { inner: root, seam });
    }
    let pre_runs = Arc::clone(&shared.runs);
    let pre = FunctionNode::new("pre", move |_| {
        let runs = Arc::clone(&pre_runs);
        async move {
            runs.root["pre"].fetch_add(1, Ordering::SeqCst);
            Ok(NodeOutput::new().with_update("pre_done", json!(true)))
        }
    });
    let post_runs = Arc::clone(&shared.runs);
    let post = FunctionNode::new("post", move |context| {
        let runs = Arc::clone(&post_runs);
        async move {
            runs.root["post"].fetch_add(1, Ordering::SeqCst);
            Ok(NodeOutput::new().with_update("post_seen", context.state["gathered"].clone()))
        }
    });
    let gather = DurableParallelNode::new(definition(max_concurrency), runtime)
        .with_decisions(shared.ledger.client(turn.claim_attempt))
        .with_events(shared.raised.clone());
    let graph = StateGraph::with_channels(&["input", "pre_done", "gathered", "post_seen"])
        .add_node(pre)
        .add_node(gather)
        .add_node(post)
        .add_edge(START, "pre")
        .add_edge("pre", "gather")
        .add_edge("gather", "post")
        .add_edge("post", END)
        .compile()
        .expect("compile the root pipeline graph")
        .with_checkpointer_arc(root)
        .with_strict_channels();
    Process { postgres, graph }
}

fn fresh_input() -> State {
    HashMap::from([("input".to_owned(), json!("original"))])
}

/// The root graph parked inside `gather`; return the one open card's key.
fn expect_park(result: Result<State, GraphError>) -> (usize, String) {
    let paused = match result {
        Err(GraphError::Interrupted(paused)) => paused,
        other => panic!("expected the root graph to park inside gather: {other:?}"),
    };
    let Interrupt::Dynamic {
        data: Some(data), ..
    } = &paused.interrupt
    else {
        panic!("expected a dynamic interrupt from the gather node");
    };
    assert_eq!(data["schema"], FANOUT_PARKED_SCHEMA);
    let pending = data["pending"].as_array().expect("parked cards");
    assert_eq!(pending.len(), 1, "exactly the one open card, no duplicate");
    assert_eq!(pending[0]["interrupt_id"], "card-ask");
    (
        paused.step,
        pending[0]["interrupt_key"]
            .as_str()
            .expect("interrupt key")
            .to_owned(),
    )
}

async fn root_frontier(postgres: &Arc<dyn Checkpointer>) -> (usize, Vec<String>) {
    let latest = postgres
        .load(ROOT)
        .await
        .expect("load the root frontier")
        .expect("a root checkpoint exists");
    (latest.step, latest.pending_nodes)
}

fn assert_completed(result: Result<State, GraphError>) {
    let state = result.expect("the continuation completes the root graph");
    let gathered = state["gathered"]
        .as_array()
        .expect("joined parallel result");
    assert_eq!(
        gathered
            .iter()
            .map(|entry| entry["outputs"]["output"].clone())
            .collect::<Vec<_>>(),
        vec![json!("done"), json!("ask"), json!("late")]
    );
    assert_eq!(state["post_seen"], state["gathered"]);
}

/// Exactly one card was ever raised, it was acknowledged `applied` exactly
/// once, no DECIDED row is left, and every child's latest row is terminal.
async fn assert_one_card_settled(database: &IsolatedPostgres, shared: &Shared, key: &str) {
    assert_eq!(
        shared.raised.distinct(),
        BTreeSet::from([key.to_owned()]),
        "a duplicate card was raised"
    );
    let applied = shared
        .ledger
        .acks()
        .iter()
        .filter(|(acked, result)| acked == key && matches!(result, AckResult::Applied { .. }))
        .count();
    assert_eq!(applied, 1, "{:?}", shared.ledger.acks());
    assert_eq!(shared.ledger.open_decisions(), 0, "an orphan DECIDED row");
    let open_children: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints c \
         WHERE thread_id <> $1 \
           AND save_ordinal = (SELECT max(save_ordinal) \
                               FROM elitea_runtime.agent_graph_checkpoints m \
                               WHERE m.thread_id = c.thread_id) \
           AND pending_nodes <> '[]'",
    )
    .bind(ROOT)
    .fetch_one(&database.pool)
    .await
    .expect("inspect the latest child checkpoints");
    assert_eq!(open_children, 0, "a child row was left unsettled");
}

fn skip_without_postgres() -> Option<String> {
    let url = crate::test_pg::database_url_var(TEST_DATABASE_URL).ok();
    if url.is_none() {
        eprintln!("skipping PostgreSQL fan-out re-entry proof: set {TEST_DATABASE_URL}");
    }
    url
}

async fn fresh_database(database_url: &str) -> IsolatedPostgres {
    let database = IsolatedPostgres::create(database_url).await;
    install_test_schema(&database.pool).await;
    database
}

/// Execution 1 (claim 1): `done` and `late` complete, `ask` parks the root.
async fn park_first(database: &IsolatedPostgres, shared: &Shared) -> (usize, String) {
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(database, &first, shared, 3, ProcessSeams::default()).await;
    let parked = expect_park(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(shared.runs.snapshot(), (1, 0, 1, 1, 1, 1));
    assert_eq!(
        root_frontier(&process.postgres).await,
        (parked.0, vec!["gather".to_owned()])
    );
    parked
}

/// Run `process` until `reached`, then kill it (abort the task).
async fn kill_at(process: Process, input: State, reached: &Notify) {
    let running = tokio::spawn(async move {
        let _ = process
            .graph
            .invoke(input, ExecutionConfig::new(ROOT))
            .await;
    });
    tokio::time::timeout(Duration::from_secs(20), reached.notified())
        .await
        .expect("the process reached its crash seam");
    running.abort();
    assert!(running.await.expect_err("process killed").is_cancelled());
}

/// Park, process replacement, a `fanout_wake` continuation with nothing
/// decided, then a continuation whose restore fetch carries the decision.
/// `gather` re-enters twice; nothing completed runs again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parked_gather_reenters_in_new_executions_and_resumes_only_the_paused_child() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let (step, key) = park_first(&database, &shared).await;

    // Execution 2 (claim 2): a wake with nothing decided re-enters `gather`,
    // restores every child from its receipt and parks again on the same card.
    let wake = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 2,
        resume: true,
    };
    let process = start_process(&database, &wake, &shared, 3, ProcessSeams::default()).await;
    let fetches = shared.ledger.fetches();
    let (wake_step, wake_key) = expect_park(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(wake_step, step, "gather re-entered at another step");
    assert_eq!(
        wake_key, key,
        "the re-entered node froze a second occurrence"
    );
    assert_eq!(
        shared.ledger.fetches(),
        fetches + 2,
        "one restore fetch and one final fetch before parking"
    );
    assert_eq!(
        shared.runs.snapshot(),
        (1, 0, 1, 1, 1, 1),
        "a wake without a decision invoked completed or paused work again"
    );
    drop(process);

    // Execution 3 (claim 3): the decision reaches only `ask`, which resumes
    // from its own checkpoint (its `prep` does not run again).
    shared.ledger.decide(&key, "approve", None);
    let answer = Turn {
        execution_id: "execution-3",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &answer, &shared, 3, ProcessSeams::default()).await;
    assert_completed(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(
        shared.runs.snapshot(),
        (1, 1, 1, 2, 1, 1),
        "(pre, post, done, ask, ask prep, late): only the paused child's owned node ran again"
    );
    assert_eq!(
        root_frontier(&process.postgres).await.1,
        Vec::<String>::new()
    );
    assert_one_card_settled(&database, &shared, &key).await;
}

/// The process dies inside `gather` after `done` completed and `ask` paused,
/// before the root pause row. A second claim of the SAME execution (lease
/// reclaim, `resume: false`) re-enters `gather` from the freeze row: `pre` and
/// the settled children are not invoked again; only the in-flight `late` is.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_second_claim_reenters_gather_after_a_crash_inside_the_node() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();

    // Concurrency 1 settles `done` and `ask` durably before `late` starts.
    let started = Arc::new(Notify::new());
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(
        &database,
        &first,
        &shared,
        1,
        ProcessSeams {
            holds: Holds {
                late_started: Some(Arc::clone(&started)),
                resumed_ask: None,
            },
            ..ProcessSeams::default()
        },
    )
    .await;
    kill_at(process, fresh_input(), &started).await;
    assert_eq!(shared.runs.snapshot(), (1, 0, 1, 1, 1, 1));

    let reclaim = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 2,
        resume: false,
    };
    let process = start_process(&database, &reclaim, &shared, 1, ProcessSeams::default()).await;
    let (crash_step, crash_pending) = root_frontier(&process.postgres).await;
    assert_eq!(
        crash_pending,
        vec!["gather".to_owned()],
        "the freeze row is the frontier"
    );
    let (step, key) = expect_park(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(step, crash_step);
    assert_eq!(
        shared.runs.snapshot(),
        (1, 0, 1, 1, 1, 2),
        "(pre, post, done, ask, ask prep, late): only the in-flight child ran again"
    );
    drop(process);

    shared.ledger.decide(&key, "approve", None);
    let answer = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &answer, &shared, 1, ProcessSeams::default()).await;
    assert_completed(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(shared.runs.snapshot(), (1, 1, 1, 2, 1, 2));
    assert_one_card_settled(&database, &shared, &key).await;
}

/// Node recovery bumps the generation of the same execution and starts the
/// pipeline as `PipelineNativeStart::Checkpoint` (`agents/pipeline.rs:1196`),
/// which `session.rs:1690` maps to `resume: false`. `TurnCheckpointer` then
/// hides every frontier of generation 1, so ADK starts the ROOT graph afresh:
/// `pre` runs again. `gather` itself still recovers its frozen occurrence (the
/// freeze reads the unfiltered store), so children are not re-invoked and the
/// card keeps its key.
///
/// This records today's behaviour as a deliberate observation for 2a-2 task 6
/// (`FanoutTurnAuthority`): a generation bump must restore the root frontier
/// (node-recovery receipts currently mask the upstream re-run for journaled
/// nodes only).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn generation_bump_restarts_the_root_frontier_but_keeps_fanout_lineage() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let (step, key) = park_first(&database, &shared).await;

    let bumped = Turn {
        execution_id: "execution-1",
        generation: 2,
        claim_attempt: 2,
        resume: false,
    };
    let process = start_process(&database, &bumped, &shared, 3, ProcessSeams::default()).await;
    let (bumped_step, bumped_key) = expect_park(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(bumped_step, step);
    assert_eq!(bumped_key, key);
    assert_eq!(
        shared.runs.snapshot(),
        (2, 0, 1, 1, 1, 1),
        "(pre, post, done, ask, ask prep, late): the root restarted, the children did not"
    );
}

/// Where the decision-carrying process dies (design §8).
#[derive(Clone, Copy, Debug)]
enum CrashSeam {
    /// The fetch returned the decision to nobody.
    AfterFetch,
    /// The resumed child saved its terminal receipt; nothing after ran.
    AfterApply,
    /// The resumed child saved; the ACK never reached Main.
    BeforeAck,
    /// Every child is terminal and acknowledged; the join row is not saved.
    BeforeJoin,
}

/// Park, decide, kill the continuation at `seam`, then a replacement
/// process (a new execution with the next claim) finishes: one final answer,
/// one card, no orphan row, completed children never invoked again.
async fn decision_crash_seam(seam: CrashSeam) {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let (_, key) = park_first(&database, &shared).await;
    shared.ledger.decide(&key, "approve", None);

    let crashing = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 2,
        resume: true,
    };
    let point = Seam::armed();
    let mut seams = ProcessSeams::default();
    match seam {
        CrashSeam::AfterFetch => shared.ledger.crash_at(LedgerSeam::AfterFetch),
        CrashSeam::BeforeAck => shared.ledger.crash_at(LedgerSeam::BeforeAck),
        CrashSeam::AfterApply => seams.child = Some(Arc::clone(&point)),
        CrashSeam::BeforeJoin => seams.root = Some(Arc::clone(&point)),
    }
    let process = start_process(&database, &crashing, &shared, 3, seams).await;
    let reached = match seam {
        CrashSeam::AfterFetch | CrashSeam::BeforeAck => &shared.ledger.reached,
        CrashSeam::AfterApply | CrashSeam::BeforeJoin => &point.reached,
    };
    kill_at(process, State::new(), reached).await;
    let resumed_before_crash = match seam {
        CrashSeam::AfterFetch => 1,
        CrashSeam::AfterApply | CrashSeam::BeforeAck | CrashSeam::BeforeJoin => 2,
    };
    assert_eq!(
        shared.runs.snapshot(),
        (1, 0, 1, resumed_before_crash, 1, 1),
        "{seam:?}: work before the crash"
    );

    let replacement = Turn {
        execution_id: "execution-3",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &replacement, &shared, 3, ProcessSeams::default()).await;
    assert_completed(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(
        shared.runs.snapshot(),
        (1, 1, 1, 2, 1, 1),
        "{seam:?}: (pre, post, done, ask, ask prep, late): the decision applied exactly once"
    );
    assert_one_card_settled(&database, &shared, &key).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_crash_after_the_fetch_applies_the_decision_once_under_the_next_claim() {
    decision_crash_seam(CrashSeam::AfterFetch).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_crash_after_the_apply_acknowledges_without_a_second_resume() {
    decision_crash_seam(CrashSeam::AfterApply).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_crash_before_the_ack_acknowledges_without_a_second_resume() {
    decision_crash_seam(CrashSeam::BeforeAck).await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_crash_before_the_join_joins_from_receipts_under_the_next_claim() {
    decision_crash_seam(CrashSeam::BeforeJoin).await;
}

/// `after_child_receipt`: the first process dies right after `done`'s
/// terminal receipt. A second claim of the same execution never runs `done`
/// again; the rest of the activation and the decision proceed as usual.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_crash_after_a_child_receipt_never_runs_that_child_again() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let point = Seam::armed();
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(
        &database,
        &first,
        &shared,
        1,
        ProcessSeams {
            child: Some(Arc::clone(&point)),
            ..ProcessSeams::default()
        },
    )
    .await;
    kill_at(process, fresh_input(), &point.reached).await;
    assert_eq!(shared.runs.snapshot(), (1, 0, 1, 0, 0, 0));

    let reclaim = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 2,
        resume: false,
    };
    let process = start_process(&database, &reclaim, &shared, 1, ProcessSeams::default()).await;
    let (_, key) = expect_park(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(shared.runs.snapshot(), (1, 0, 1, 1, 1, 1));
    drop(process);
    shared.ledger.decide(&key, "approve", None);
    let answer = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &answer, &shared, 1, ProcessSeams::default()).await;
    assert_completed(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(shared.runs.snapshot(), (1, 1, 1, 2, 1, 1));
    assert_one_card_settled(&database, &shared, &key).await;
}

/// Two claims race for one parked activation with one decision: the later
/// claim fences the earlier one in `PostgreSQL` and in the ledger, so the
/// decision applies exactly once and only the later claim answers.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_two_claim_race_applies_the_decision_once() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let (_, key) = park_first(&database, &shared).await;
    shared.ledger.decide(&key, "approve", None);
    let earlier = start_process(
        &database,
        &Turn {
            execution_id: "execution-2",
            generation: 1,
            claim_attempt: 2,
            resume: true,
        },
        &shared,
        3,
        ProcessSeams::default(),
    )
    .await;
    let later = start_process(
        &database,
        &Turn {
            execution_id: "execution-3",
            generation: 1,
            claim_attempt: 3,
            resume: true,
        },
        &shared,
        3,
        ProcessSeams::default(),
    )
    .await;
    let (earlier, later) = tokio::join!(
        earlier
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT)),
        later.graph.invoke(State::new(), ExecutionConfig::new(ROOT)),
    );
    let error = earlier.expect_err("the superseded claim must not answer");
    assert!(error.to_string().contains("writer_not_current"), "{error}");
    assert_completed(later);
    assert_eq!(shared.runs.root("post"), 1);
    assert_eq!(
        (shared.runs.work("done"), shared.runs.work("late")),
        (1, 1),
        "a completed child ran again"
    );
    assert_one_card_settled(&database, &shared, &key).await;
}

/// A zombie (superseded) claim is mid-apply when the later claim applies the
/// same decision and completes. The zombie's child save is fenced: it reports
/// lease loss, never an answer, and the later claim's checkpoints stay latest.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_zombie_claims_save_is_fenced_after_the_later_claim_applied() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let (_, key) = park_first(&database, &shared).await;
    shared.ledger.decide(&key, "approve", None);

    let entered = Arc::new(Notify::new());
    let release = Arc::new(Notify::new());
    let zombie = start_process(
        &database,
        &Turn {
            execution_id: "execution-2",
            generation: 1,
            claim_attempt: 2,
            resume: true,
        },
        &shared,
        3,
        ProcessSeams {
            holds: Holds {
                late_started: None,
                resumed_ask: Some((Arc::clone(&entered), Arc::clone(&release))),
            },
            ..ProcessSeams::default()
        },
    )
    .await;
    let zombie = tokio::spawn(async move {
        zombie
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await
    });
    tokio::time::timeout(Duration::from_secs(20), entered.notified())
        .await
        .expect("the zombie resumed ask");

    let later = start_process(
        &database,
        &Turn {
            execution_id: "execution-3",
            generation: 1,
            claim_attempt: 3,
            resume: true,
        },
        &shared,
        3,
        ProcessSeams::default(),
    )
    .await;
    assert_completed(
        later
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    release.notify_one();
    let error = zombie
        .await
        .expect("zombie task")
        .expect_err("the zombie must not answer");
    assert!(error.to_string().contains("writer_not_current"), "{error}");
    assert_eq!(shared.runs.root("post"), 1);
    assert_one_card_settled(&database, &shared, &key).await;
    let foreign_latest: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM elitea_runtime.agent_graph_checkpoints c \
         WHERE thread_id <> $1 \
           AND save_ordinal = (SELECT max(save_ordinal) \
                               FROM elitea_runtime.agent_graph_checkpoints m \
                               WHERE m.thread_id = c.thread_id) \
           AND writer_execution_id = 'execution-2'",
    )
    .bind(ROOT)
    .fetch_one(&database.pool)
    .await
    .expect("inspect the latest child checkpoints");
    assert_eq!(
        foreign_latest, 0,
        "a zombie write became a child's latest row"
    );
}

/// The fan-out turn boundary on real `PostgreSQL` (spike finding 2): the
/// parent chain is `bind_parallel_authority`'s (occurrence ->
/// `FanoutTurnAuthority` -> the production holder), not `TurnCheckpointer`.
/// Every root row, the occurrence freeze and join rows included, names the
/// execution that wrote it; a `fanout_wake` (explicit resume, empty input)
/// re-enters the parked node through it and nothing completed runs again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_fanout_turn_authority_stamps_every_root_row_across_park_wake_and_answer() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = fresh_database(&database_url).await;
    let shared = Shared::new();
    let seams = || ProcessSeams {
        turn_authority: true,
        ..ProcessSeams::default()
    };
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(&database, &first, &shared, 3, seams()).await;
    let (step, key) = expect_park(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(shared.runs.snapshot(), (1, 0, 1, 1, 1, 1));
    drop(process);

    // The admitted `FanoutWake` start is an explicit resume with empty input.
    let wake = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 2,
        resume: true,
    };
    let process = start_process(&database, &wake, &shared, 3, seams()).await;
    let (wake_step, wake_key) = expect_park(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!((wake_step, wake_key.as_str()), (step, key.as_str()));
    assert_eq!(shared.runs.snapshot(), (1, 0, 1, 1, 1, 1));
    drop(process);

    shared.ledger.decide(&key, "approve", None);
    let answer = Turn {
        execution_id: "execution-3",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &answer, &shared, 3, seams()).await;
    assert_completed(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(shared.runs.snapshot(), (1, 1, 1, 2, 1, 1));
    assert_one_card_settled(&database, &shared, &key).await;

    let rows = sqlx::query_as::<_, (String, i64, String)>(
        "SELECT writer_execution_id, writer_generation, metadata \
         FROM elitea_runtime.agent_graph_checkpoints WHERE thread_id = $1 ORDER BY save_ordinal",
    )
    .bind(ROOT)
    .fetch_all(&database.pool)
    .await
    .expect("read the root rows");
    let mut writers = BTreeSet::new();
    let mut occurrence_rows = 0;
    for (execution, generation, metadata) in &rows {
        let metadata: Value = serde_json::from_str(metadata).expect("root metadata");
        assert_eq!(
            metadata["elitea.pipeline.execution.v1"],
            json!([execution, generation]),
            "a root row without its writer's execution stamp: {metadata}"
        );
        if metadata
            .get("elitea.graph.parallel.occurrence.v3")
            .is_some()
        {
            occurrence_rows += 1;
        }
        writers.insert(execution.clone());
    }
    assert!(
        occurrence_rows >= 1,
        "the freeze row is among the stamped rows"
    );
    assert_eq!(
        writers,
        BTreeSet::from([
            "execution-1".to_owned(),
            "execution-2".to_owned(),
            "execution-3".to_owned()
        ])
    );
}
