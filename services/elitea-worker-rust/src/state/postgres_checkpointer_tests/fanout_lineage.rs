//! Fan-out child lineage survives a new execution, a later claim and a resume.
//!
//! A HITL continuation or a reclaim is a new execution with its own claim.
//! The children it restores must be the children the occurrence froze, so a
//! completed child never runs again and a paused child resumes from its own
//! checkpoint.
use std::collections::BTreeMap;

use super::*;
use crate::agents::graph::map_reduce::{
    DurableMapNode, MapDefinition, MapItemExecution, MapNodeOutcome, MapOccurrenceCheckpointer,
    MapReduction, MapWorkerGraphFactory, MapWorkerKind, MapWorkerTerminal,
};
use crate::agents::graph::{
    AdkParallelBranchRuntime, DurableParallelNode, PARALLEL_INTERRUPT_SCHEMA,
    PARALLEL_RESUME_STATE_KEY, ParallelBranchDefinition, ParallelBranchExecution,
    ParallelBranchGraphFactory, ParallelBranchPause, ParallelBranchRuntime, ParallelBranchTerminal,
    ParallelCheckpointAppender, ParallelDecision, ParallelOccurrenceCheckpointer,
    ParallelPauseCard, PreparedParallelActivation,
};
use adk_rust::graph::{
    CompiledGraph, FunctionNode, GraphError, Node, NodeContext, StateSchema, interrupt::Interrupt,
};
use async_trait::async_trait;
use serde_json::Value;
use tokio::sync::Notify;

const ROOT: &str = "thread-1";
const DEFINITION: [u8; 32] = [0x51; 32];
const RESUME_KEY: &str = "__elitea_hitl_resume_v1";

/// Count each node of each branch. `prep` runs once before the owned node, so a
/// resume that restarts the branch instead of restoring it shows up here.
#[derive(Default)]
struct BranchRuns {
    prep: BTreeMap<String, AtomicUsize>,
    work: BTreeMap<String, AtomicUsize>,
}

impl BranchRuns {
    fn new(names: &[&str]) -> Self {
        Self {
            prep: names
                .iter()
                .map(|name| ((*name).to_owned(), AtomicUsize::new(0)))
                .collect(),
            work: names
                .iter()
                .map(|name| ((*name).to_owned(), AtomicUsize::new(0)))
                .collect(),
        }
    }

    fn prep(&self, name: &str) -> usize {
        self.prep[name].load(Ordering::SeqCst)
    }

    fn work(&self, name: &str) -> usize {
        self.work[name].load(Ordering::SeqCst)
    }
}

struct LineageBranches {
    runs: Arc<BranchRuns>,
    pausing: &'static str,
}

#[async_trait]
impl ParallelBranchGraphFactory for LineageBranches {
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
        let pauses = name == self.pausing;
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
        let work = FunctionNode::new(&name, move |context| {
            let runs = Arc::clone(&work_runs);
            let name = work_name.clone();
            async move {
                runs.work[&name].fetch_add(1, Ordering::SeqCst);
                if pauses && !context.state.contains_key(RESUME_KEY) {
                    return Ok(NodeOutput::interrupt_with_data(
                        "Child paused.",
                        json!({"interrupt_id": format!("card-{name}"), "tool_call_id": format!("call-{name}")}),
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

async fn claim(
    database: &IsolatedPostgres,
    execution_id: &str,
    claim_attempt: u64,
    lease: Arc<TestStateWriterLease>,
) -> Arc<PostgresCheckpointer> {
    Arc::new(
        PostgresCheckpointer::activate(
            database.pool.clone(),
            writer(
                execution_id,
                &format!("claim-{execution_id}"),
                claim_attempt,
                claim_attempt,
                DEFINITION,
                [0x61; 32],
            ),
            CheckpointLimits::default(),
            lease,
        )
        .await
        .expect("activate the execution's root checkpoint writer"),
    )
}

fn parallel_runtime(
    checkpointer: &Arc<PostgresCheckpointer>,
    runs: &Arc<BranchRuns>,
) -> Arc<AdkParallelBranchRuntime> {
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpointer.clone();
    Arc::new(AdkParallelBranchRuntime::new(
        checkpointer.clone(),
        Arc::new(LineageBranches {
            runs: Arc::clone(runs),
            pausing: "ask",
        }),
        Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
    ))
}

fn four_branches() -> ParallelNodeDefinition {
    ParallelNodeDefinition::from_yaml(
        r"
id: gather
type: parallel
branches:
  - id: first
    node: done_a
  - id: second
    node: done_b
  - id: third
    node: ask
  - id: fourth
    node: late
max_concurrency: 4
wait: all
error_policy: fail_after_drain
output: [gathered]
transition: END
        ",
    )
    .expect("valid four-branch parallel fixture")
}

fn parallel_context(state: State) -> NodeContext {
    NodeContext::new(state, ExecutionConfig::new(ROOT), 4)
}

fn original_state() -> State {
    HashMap::from([("input".to_owned(), json!("original"))])
}

fn aggregate_pause(output: &NodeOutput) -> Value {
    let Some(Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt.as_ref()
    else {
        panic!("expected the aggregate parallel pause");
    };
    assert_eq!(data["schema"], PARALLEL_INTERRUPT_SCHEMA);
    data.clone()
}

fn approve_all(pause: &Value) -> State {
    let decisions = pause["cards"]
        .as_array()
        .expect("aggregate cards")
        .iter()
        .map(|card| {
            json!({
                "interrupt_id": card["interrupt_id"],
                "tool_call_id": card["tool_call_id"],
                "action": "approve",
                "value": "",
            })
        })
        .collect::<Vec<_>>();
    HashMap::from([
        ("input".to_owned(), json!("original")),
        (
            PARALLEL_RESUME_STATE_KEY.to_owned(),
            json!({
                "schema": PARALLEL_INTERRUPT_SCHEMA,
                "parallel_activation": pause["parallel_activation"],
                "decisions": decisions,
            }),
        ),
    ])
}

/// C1a. Freeze four branches under execution 1: two complete, one pauses and
/// the process dies before the fourth is admitted. Execution 2 (a later claim)
/// restores the occurrence; execution 3 is the HITL continuation.
#[tokio::test]
async fn parallel_children_survive_a_new_execution_and_resume_from_their_own_checkpoint() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL fan-out lineage test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let runs = Arc::new(BranchRuns::new(&["done_a", "done_b", "ask", "late"]));
    let definition = four_branches();

    let first = claim(
        &database,
        "execution-1",
        1,
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let first_runtime = parallel_runtime(&first, &runs);
    let context = parallel_context(original_state());
    let mut activation = ParallelActivation {
        root_thread_id: ROOT.to_owned(),
        node_id: definition.id().to_owned(),
        step: 4,
        config_digest: definition.config_digest(),
    };
    let PreparedParallelActivation::Ready(prepared) = first_runtime
        .prepare(&mut activation, &definition, &context)
        .await
        .expect("freeze the occurrence under execution 1")
    else {
        panic!("unexpected blocked occurrence");
    };
    for branch in prepared.into_iter().take(3) {
        let _ = first_runtime.invoke(&activation, branch, &context).await;
    }
    drop(first_runtime);
    assert_eq!(
        (
            runs.work("done_a"),
            runs.work("done_b"),
            runs.work("ask"),
            runs.work("late")
        ),
        (1, 1, 1, 0)
    );

    // Execution 2 takes over the same root thread with a later claim.
    let second = claim(
        &database,
        "execution-2",
        2,
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let restored = DurableParallelNode::new(definition.clone(), parallel_runtime(&second, &runs))
        .execute(&context)
        .await
        .expect("restore the occurrence under execution 2");
    let pause = aggregate_pause(&restored);
    assert_eq!(pause["cards"].as_array().map(Vec::len), Some(1));
    assert_eq!(pause["cards"][0]["interrupt_id"], "card-ask");
    assert_eq!(runs.work("done_a"), 1, "a completed branch ran again");
    assert_eq!(runs.work("done_b"), 1, "a completed branch ran again");
    assert_eq!(runs.work("ask"), 1, "the paused branch lost its checkpoint");
    assert_eq!(runs.work("late"), 1);

    // Execution 3 is the HITL continuation that carries the decision.
    let third = claim(
        &database,
        "execution-3",
        3,
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let resumed = DurableParallelNode::new(definition, parallel_runtime(&third, &runs))
        .execute(&parallel_context(approve_all(&pause)))
        .await
        .expect("resume the paused branch under execution 3");
    assert!(resumed.interrupt.is_none());
    let gathered = resumed.updates["gathered"]
        .as_array()
        .expect("joined parallel result");
    assert_eq!(
        gathered
            .iter()
            .map(|entry| entry["outputs"]["output"].clone())
            .collect::<Vec<_>>(),
        vec![
            json!("done_a"),
            json!("done_b"),
            json!("ask"),
            json!("late")
        ]
    );
    assert_eq!(gathered[2]["outputs"]["prepared"], "ask");
    for name in ["done_a", "done_b", "late"] {
        assert_eq!(runs.work(name), 1, "{name} ran again after the resume");
        assert_eq!(runs.prep(name), 1, "{name} restarted after the resume");
    }
    assert_eq!(
        runs.work("ask"),
        2,
        "the decision did not reach the paused child"
    );
    assert_eq!(
        runs.prep("ask"),
        1,
        "the paused branch restarted from scratch"
    );
}

/// A non-pausing Map worker that records each item it runs. While `hold` is
/// set, the last item parks forever after announcing that it started.
struct LineageWorkers {
    calls: Arc<std::sync::Mutex<Vec<usize>>>,
    hold: Option<(usize, Arc<Notify>)>,
}

struct LineageWorker {
    calls: Arc<std::sync::Mutex<Vec<usize>>>,
    hold: Option<(usize, Arc<Notify>)>,
}

#[async_trait]
impl Node for LineageWorker {
    fn name(&self) -> &'static str {
        "worker"
    }

    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let index = context
            .state
            .get("item_index")
            .and_then(Value::as_u64)
            .and_then(|index| usize::try_from(index).ok())
            .ok_or_else(|| GraphError::Other("item index missing".to_owned()))?;
        self.calls.lock().expect("worker call log").push(index);
        if let Some((held, started)) = &self.hold
            && *held == index
        {
            started.notify_one();
            std::future::pending::<()>().await;
        }
        Ok(NodeOutput::new().with_update("item_result", json!({"value": context.state["entity"]})))
    }
}

impl MapWorkerGraphFactory for LineageWorkers {
    fn validate_nonpausing(&self, _definition: &MapDefinition) -> Result<(), GraphError> {
        Ok(())
    }

    fn owned_definition_digest(&self) -> [u8; 32] {
        [0x19; 32]
    }

    fn worker_kind(&self) -> MapWorkerKind {
        MapWorkerKind::StateModifier
    }

    fn compile_item(
        &self,
        _definition: &MapDefinition,
        execution: &MapItemExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let worker = LineageWorker {
            calls: Arc::clone(&self.calls),
            hold: self.hold.clone(),
        };
        Ok(StateGraph::new(StateSchema::simple(&[
            "entity",
            "item_index",
            "item_result",
        ]))
        .add_node(execution.wrap_node(worker))
        .add_edge(START, "worker")
        .add_edge("worker", END)
        .compile()?
        .with_checkpointer_arc(execution.checkpointer())
        .with_max_concurrency(1))
    }

    fn runtime_input(
        &self,
        input: &State,
        _context: &NodeContext,
        _thread: &str,
    ) -> Result<State, GraphError> {
        Ok(input.clone())
    }

    fn project_result(
        &self,
        _definition: &MapDefinition,
        state: &State,
    ) -> Result<MapWorkerTerminal, GraphError> {
        let mut outputs = serde_json::Map::new();
        outputs.insert(
            "item_result".to_owned(),
            state.get("item_result").cloned().unwrap_or(Value::Null),
        );
        Ok(MapWorkerTerminal::Completed(outputs))
    }

    fn validate_candidate(&self, _candidate: &State) -> Result<(), GraphError> {
        Ok(())
    }
}

fn map_definition() -> MapDefinition {
    MapDefinition {
        id: "map_items".to_owned(),
        worker: "worker".to_owned(),
        source: "items".to_owned(),
        item: "entity".to_owned(),
        index: "item_index".to_owned(),
        broadcast: Vec::new(),
        outputs: vec!["item_result".to_owned()],
        destination: "mapped".to_owned(),
        max_items: 64,
        // One item at a time makes "items 0 and 1 saved, item 2 running" exact.
        max_concurrency: 1,
        reduction: MapReduction::OrderedCollection,
    }
}

fn map_context() -> NodeContext {
    let state = State::from([
        ("items".to_owned(), json!(["zero", "one", "two"])),
        ("mapped".to_owned(), json!([])),
    ]);
    let mut context = NodeContext::new(state, ExecutionConfig::new(ROOT), 2);
    context.set_parent_schema(Arc::new(StateSchema::simple(&["items", "mapped"])));
    context
}

fn map_node(checkpointer: &Arc<PostgresCheckpointer>, workers: LineageWorkers) -> DurableMapNode {
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpointer.clone();
    DurableMapNode::new(
        map_definition(),
        Arc::new(MapOccurrenceCheckpointer::new(parent)),
        checkpointer.clone(),
        Arc::new(workers),
    )
    .expect("valid Map fixture")
}

/// C1a (Map). Items 0 and 1 complete under execution 1 and the process dies
/// while item 2 runs. A takeover by execution 2 must not run 0 or 1 again.
#[tokio::test]
async fn map_items_completed_before_a_takeover_by_a_new_execution_do_not_run_again() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL fan-out lineage test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_test_schema(&database.pool).await;
    let calls = Arc::new(std::sync::Mutex::new(Vec::new()));
    let context = map_context();

    let first = claim(
        &database,
        "execution-1",
        1,
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let started = Arc::new(Notify::new());
    let crashed = map_node(
        &first,
        LineageWorkers {
            calls: Arc::clone(&calls),
            hold: Some((2, Arc::clone(&started))),
        },
    );
    tokio::select! {
        biased;
        () = started.notified() => {}
        outcome = crashed.execute_outcome(&context) => {
            panic!("the held item cannot finish: {:?}", outcome.is_ok());
        }
    }
    // Dropping the in-flight node future is the process loss.
    drop(crashed);
    assert_eq!(*calls.lock().expect("call log"), vec![0, 1, 2]);

    let second = claim(
        &database,
        "execution-2",
        2,
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    let outcome = map_node(
        &second,
        LineageWorkers {
            calls: Arc::clone(&calls),
            hold: None,
        },
    )
    .execute_outcome(&context)
    .await
    .expect("restore the Map occurrence under execution 2");
    let MapNodeOutcome::Completed(output) = outcome else {
        panic!("the restored Map stopped");
    };
    assert_eq!(
        output.updates["mapped"].as_array().map(|entries| entries
            .iter()
            .map(|entry| entry["outputs"]["item_result"]["value"].clone())
            .collect::<Vec<_>>()),
        Some(vec![json!("zero"), json!("one"), json!("two")])
    );
    assert_eq!(
        *calls.lock().expect("call log"),
        vec![0, 1, 2, 2],
        "completed items ran again after the takeover"
    );
}
