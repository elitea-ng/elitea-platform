//! Fan-out V2 stop-point proof (design §12.1 task 1, PLAN §5): ADK re-enters a
//! dynamically interrupted graph node on real `PostgreSQL`.
//!
//! The root pipeline graph is `pre -> gather -> post`. `gather` is the durable
//! Parallel node; one branch pauses with a dynamic interrupt, so `gather`
//! returns `NodeOutput::interrupt_with_data` and ADK saves the root frontier
//! with `gather` still pending. Each scenario then drops every in-memory object
//! (process replacement) and activates a new writer claim on the same root
//! thread. The checkpointer chain matches production composition:
//! `TurnCheckpointer -> ParallelOccurrenceCheckpointer -> PostgresCheckpointer`
//! (`agents/session.rs:1691`, `parallel_checkpoint.rs` "give this wrapper to
//! both the parent graph and the branch runtime").
//!
//! Proven per scenario: the interrupted node re-enters at the same step, the
//! completed upstream node and completed children are never invoked again, and
//! the paused child resumes from its own checkpoint (its pre-pause node does
//! not run again). Decisions still travel through the V1 aggregate resume key
//! here; V2 replaces the delivery (claim-fenced fetch) but not the re-entry.
use std::collections::BTreeMap;

use super::*;
use crate::agents::graph::turn_checkpointer::TurnCheckpointer;
use crate::agents::graph::{
    AdkParallelBranchRuntime, DurableParallelNode, PARALLEL_INTERRUPT_SCHEMA,
    PARALLEL_RESUME_STATE_KEY, ParallelBranchDefinition, ParallelBranchExecution,
    ParallelBranchGraphFactory, ParallelBranchPause, ParallelBranchTerminal,
    ParallelCheckpointAppender, ParallelDecision, ParallelOccurrenceCheckpointer,
    ParallelPauseCard,
};
use adk_rust::graph::{CompiledGraph, FunctionNode, GraphError, interrupt::Interrupt};
use async_trait::async_trait;
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

/// While set, `late` announces that it started and then never returns, so the
/// test can kill the process mid-node after `done` and `ask` have settled.
type Hold = Option<Arc<Notify>>;

struct Branches {
    runs: Arc<Runs>,
    hold: Hold,
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
        let hold = self.hold.clone().filter(|_| name == "late");
        let work = FunctionNode::new(&name, move |context| {
            let runs = Arc::clone(&work_runs);
            let name = work_name.clone();
            let hold = hold.clone();
            async move {
                runs.work[&name].fetch_add(1, Ordering::SeqCst);
                if let Some(started) = hold {
                    started.notify_one();
                    std::future::pending::<()>().await;
                }
                if name == "ask" && !context.state.contains_key(RESUME_KEY) {
                    return Ok(NodeOutput::interrupt_with_data(
                        "Child paused.",
                        json!({"interrupt_id": "card-ask", "tool_call_id": "call-ask"}),
                    ));
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
    postgres: Arc<PostgresCheckpointer>,
    graph: CompiledGraph,
}

struct Turn<'a> {
    execution_id: &'a str,
    generation: u64,
    claim_attempt: u64,
    /// The pipeline start is an explicit continuation (HITL or `fanout_wake`).
    resume: bool,
}

async fn start_process(
    database: &IsolatedPostgres,
    turn: &Turn<'_>,
    runs: &Arc<Runs>,
    max_concurrency: usize,
    hold: Hold,
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
    let appender: Arc<dyn ParallelCheckpointAppender> = postgres.clone();
    let occurrence = Arc::new(ParallelOccurrenceCheckpointer::new(appender));
    let runtime = Arc::new(AdkParallelBranchRuntime::new(
        postgres.clone(),
        Arc::new(Branches {
            runs: Arc::clone(runs),
            hold,
        }),
        Arc::clone(&occurrence),
    ));
    let root: Arc<dyn Checkpointer> = Arc::new(TurnCheckpointer::new(
        occurrence,
        turn.execution_id,
        turn.generation,
        turn.resume,
    ));
    let pre_runs = Arc::clone(runs);
    let pre = FunctionNode::new("pre", move |_| {
        let runs = Arc::clone(&pre_runs);
        async move {
            runs.root["pre"].fetch_add(1, Ordering::SeqCst);
            Ok(NodeOutput::new().with_update("pre_done", json!(true)))
        }
    });
    let post_runs = Arc::clone(runs);
    let post = FunctionNode::new("post", move |context| {
        let runs = Arc::clone(&post_runs);
        async move {
            runs.root["post"].fetch_add(1, Ordering::SeqCst);
            Ok(NodeOutput::new().with_update("post_seen", context.state["gathered"].clone()))
        }
    });
    let graph = StateGraph::with_channels(&[
        "input",
        "pre_done",
        "gathered",
        "post_seen",
        PARALLEL_RESUME_STATE_KEY,
    ])
    .add_node(pre)
    .add_node(DurableParallelNode::new(
        definition(max_concurrency),
        runtime,
    ))
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

/// The root graph paused inside `gather`; return the aggregate card payload.
fn expect_pause(result: Result<State, GraphError>) -> (usize, Value) {
    let Err(GraphError::Interrupted(paused)) = result else {
        panic!("expected the root graph to pause inside gather");
    };
    let Interrupt::Dynamic {
        data: Some(data), ..
    } = &paused.interrupt
    else {
        panic!("expected a dynamic interrupt from the gather node");
    };
    assert_eq!(data["schema"], PARALLEL_INTERRUPT_SCHEMA);
    assert_eq!(
        data["cards"].as_array().map(|cards| cards
            .iter()
            .map(|card| card["interrupt_id"].clone())
            .collect::<Vec<_>>()),
        Some(vec![json!("card-ask")]),
        "exactly the one open card, no duplicate"
    );
    (paused.step, data.clone())
}

/// The decision the continuation carries (V1 delivery through graph input).
fn approve(pause: &Value) -> State {
    HashMap::from([(
        PARALLEL_RESUME_STATE_KEY.to_owned(),
        json!({
            "schema": PARALLEL_INTERRUPT_SCHEMA,
            "parallel_activation": pause["parallel_activation"],
            "decisions": [{
                "interrupt_id": "card-ask",
                "tool_call_id": "call-ask",
                "action": "approve",
                "value": "",
            }],
        }),
    )])
}

async fn root_frontier(postgres: &PostgresCheckpointer) -> (usize, Vec<String>) {
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

fn skip_without_postgres() -> Option<String> {
    let url = crate::test_pg::database_url_var(TEST_DATABASE_URL).ok();
    if url.is_none() {
        eprintln!("skipping PostgreSQL fan-out re-entry proof: set {TEST_DATABASE_URL}");
    }
    url
}

/// Park, process replacement, `fanout_wake`-style continuation (a new
/// execution with the next claim and no decision), then the HITL continuation
/// with the decision. `gather` re-enters twice; nothing completed runs again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn parked_gather_reenters_in_new_executions_and_resumes_only_the_paused_child() {
    let Some(database_url) = skip_without_postgres() else {
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let runs = Runs::new();

    // Process 1, execution 1: the branch `ask` pauses; the root parks.
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(&database, &first, &runs, 3, None).await;
    let (step, pause) = expect_pause(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(runs.snapshot(), (1, 0, 1, 1, 1, 1));
    assert_eq!(
        root_frontier(&process.postgres).await,
        (step, vec!["gather".to_owned()])
    );
    drop(process);

    // Process 2, execution 2 (claim 2): a wake with no decision re-enters
    // `gather`, restores every child from its receipt and parks again.
    let wake = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 2,
        resume: true,
    };
    let process = start_process(&database, &wake, &runs, 3, None).await;
    let (wake_step, wake_pause) = expect_pause(
        process
            .graph
            .invoke(State::new(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(wake_step, step, "gather re-entered at another step");
    assert_eq!(
        wake_pause["parallel_activation"], pause["parallel_activation"],
        "the re-entered node froze a second occurrence"
    );
    assert_eq!(
        runs.snapshot(),
        (1, 0, 1, 1, 1, 1),
        "a wake without a decision invoked completed or paused work again"
    );
    drop(process);

    // Process 3, execution 3 (claim 3): the decision reaches only `ask`,
    // which resumes from its own checkpoint (its `prep` does not run again).
    let answer = Turn {
        execution_id: "execution-3",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &answer, &runs, 3, None).await;
    assert_completed(
        process
            .graph
            .invoke(approve(&pause), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(
        runs.snapshot(),
        (1, 1, 1, 2, 1, 1),
        "(pre, post, done, ask, ask prep, late): only the paused child's owned node ran again"
    );
    assert_eq!(
        root_frontier(&process.postgres).await.1,
        Vec::<String>::new()
    );
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
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let runs = Runs::new();

    // Concurrency 1 settles `done` and `ask` durably before `late` starts.
    let started = Arc::new(Notify::new());
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(&database, &first, &runs, 1, Some(Arc::clone(&started))).await;
    let running = tokio::spawn(async move {
        let _ = process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await;
    });
    tokio::time::timeout(Duration::from_secs(10), started.notified())
        .await
        .expect("late branch started");
    running.abort();
    assert!(running.await.expect_err("process killed").is_cancelled());
    assert_eq!(runs.snapshot(), (1, 0, 1, 1, 1, 1));

    let reclaim = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 2,
        resume: false,
    };
    let process = start_process(&database, &reclaim, &runs, 1, None).await;
    let (crash_step, crash_pending) = root_frontier(&process.postgres).await;
    assert_eq!(
        crash_pending,
        vec!["gather".to_owned()],
        "the freeze row is the frontier"
    );
    let (step, pause) = expect_pause(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(step, crash_step);
    assert_eq!(
        runs.snapshot(),
        (1, 0, 1, 1, 1, 2),
        "(pre, post, done, ask, ask prep, late): only the in-flight child ran again"
    );
    drop(process);

    let answer = Turn {
        execution_id: "execution-2",
        generation: 1,
        claim_attempt: 3,
        resume: true,
    };
    let process = start_process(&database, &answer, &runs, 1, None).await;
    assert_completed(
        process
            .graph
            .invoke(approve(&pause), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(runs.snapshot(), (1, 1, 1, 2, 1, 2));
}

/// Node recovery bumps the generation of the same execution and starts the
/// pipeline as `PipelineNativeStart::Checkpoint` (`agents/pipeline.rs:1196`),
/// which `session.rs:1690` maps to `resume: false`. `TurnCheckpointer` then
/// hides every frontier of generation 1, so ADK starts the ROOT graph afresh:
/// `pre` runs again. `gather` itself still recovers its frozen occurrence (the
/// freeze reads the unfiltered store), so children are not re-invoked.
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
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let runs = Runs::new();
    let first = Turn {
        execution_id: "execution-1",
        generation: 1,
        claim_attempt: 1,
        resume: false,
    };
    let process = start_process(&database, &first, &runs, 3, None).await;
    let (step, pause) = expect_pause(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    drop(process);

    let bumped = Turn {
        execution_id: "execution-1",
        generation: 2,
        claim_attempt: 2,
        resume: false,
    };
    let process = start_process(&database, &bumped, &runs, 3, None).await;
    let (bumped_step, bumped_pause) = expect_pause(
        process
            .graph
            .invoke(fresh_input(), ExecutionConfig::new(ROOT))
            .await,
    );
    assert_eq!(bumped_step, step);
    assert_eq!(
        bumped_pause["parallel_activation"],
        pause["parallel_activation"]
    );
    assert_eq!(
        runs.snapshot(),
        (2, 0, 1, 1, 1, 1),
        "(pre, post, done, ask, ask prep, late): the root restarted, the children did not"
    );
}
