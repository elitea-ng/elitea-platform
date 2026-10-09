use std::collections::{BTreeSet, HashMap};
use std::fmt::Write as _;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use adk_rust::graph::checkpoint::{Checkpointer, MemoryCheckpointer};
use adk_rust::graph::{
    Checkpoint, CompiledGraph, ExecutionConfig, FunctionNode, GraphError, Node, NodeContext,
    NodeOutput, START, State, StateGraph,
};
use async_trait::async_trait;
use serde_json::{Value, json};
use tokio::sync::{Mutex, Notify, Semaphore, mpsc};

use super::fanout_control::FanoutCancellation;
use super::parallel::{
    AdkParallelBranchRuntime, DurableParallelNode, PARALLEL_INTERRUPT_SCHEMA,
    PARALLEL_RESUME_STATE_KEY, ParallelActivation, ParallelBlocked, ParallelBranchExecution,
    ParallelBranchGraphFactory, ParallelBranchPause, ParallelBranchTerminal,
    ParallelCheckpointAppender, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildOrigin, ParallelDecision, ParallelNodeOutcome, ParallelOccurrenceCheckpointer,
    ParallelPauseCard, PreparedParallelActivation, PreparedParallelBranch, projected_input_digest,
};
use super::{ParallelBranchDefinition, ParallelNodeDefinition};

type BehaviorFuture = Pin<Box<dyn Future<Output = Result<Value, GraphError>> + Send>>;
type Behavior = Arc<dyn Fn() -> BehaviorFuture + Send + Sync>;

#[derive(Default)]
struct AtomicParentRows {
    checkpoints: Vec<Checkpoint>,
    before_append: Option<Checkpoint>,
}

#[derive(Default)]
struct AtomicParentCheckpoints {
    rows: Mutex<AtomicParentRows>,
}

fn checkpoint_equal(left: &Checkpoint, right: &Checkpoint) -> Result<bool, GraphError> {
    Ok(serde_json::to_value(left)? == serde_json::to_value(right)?)
}

fn insert_immutable(
    rows: &mut AtomicParentRows,
    checkpoint: &Checkpoint,
) -> Result<String, GraphError> {
    if let Some(existing) = rows
        .checkpoints
        .iter()
        .find(|row| row.checkpoint_id == checkpoint.checkpoint_id)
    {
        return if checkpoint_equal(existing, checkpoint)? {
            Ok(existing.checkpoint_id.clone())
        } else {
            Err(GraphError::CheckpointError(
                "immutable checkpoint conflict".to_owned(),
            ))
        };
    }
    rows.checkpoints.push(checkpoint.clone());
    Ok(checkpoint.checkpoint_id.clone())
}

#[async_trait]
impl Checkpointer for AtomicParentCheckpoints {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let mut rows = self.rows.lock().await;
        insert_immutable(&mut rows, checkpoint)
    }

    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .await
            .checkpoints
            .iter()
            .rev()
            .find(|row| row.thread_id == thread)
            .cloned())
    }

    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .await
            .checkpoints
            .iter()
            .find(|row| row.checkpoint_id == id)
            .cloned())
    }

    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .await
            .checkpoints
            .iter()
            .filter(|row| row.thread_id == thread)
            .cloned()
            .collect())
    }

    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.rows
            .lock()
            .await
            .checkpoints
            .retain(|row| row.thread_id != thread);
        Ok(())
    }
}

#[async_trait]
impl ParallelCheckpointAppender for AtomicParentCheckpoints {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let mut rows = self.rows.lock().await;
        // This models a competing committed writer after the wrapper's load and
        // before its atomic comparison. Ordinary writer fencing is insufficient.
        if let Some(competing) = rows.before_append.take() {
            insert_immutable(&mut rows, &competing)?;
        }
        if rows
            .checkpoints
            .iter()
            .any(|row| row.checkpoint_id == candidate.checkpoint_id)
        {
            return insert_immutable(&mut rows, candidate);
        }
        let latest = rows
            .checkpoints
            .iter()
            .rev()
            .find(|row| row.thread_id == candidate.thread_id);
        let matches = match (expected, latest) {
            (Some(expected), Some(latest)) => checkpoint_equal(expected, latest)?,
            (None, None) => true,
            _ => false,
        };
        if !matches {
            return Err(GraphError::CheckpointError(
                "atomic expected-parent conflict".to_owned(),
            ));
        }
        insert_immutable(&mut rows, candidate)
    }
}

#[derive(Default)]
struct MemoryChildCheckpoints {
    stores: Mutex<HashMap<String, Arc<MemoryCheckpointer>>>,
    issued_threads: Mutex<Vec<String>>,
    parent: Arc<AtomicParentCheckpoints>,
    /// Branch ID whose child writer reports `checkpoint.writer_not_current` on save.
    lease_lost_branch: Option<&'static str>,
}

const LEASE_LOST: &str =
    "checkpoint.writer_not_current: the PostgreSQL checkpoint writer is no longer current";

/// A child store whose writer fence has been lost: reads work, every save is refused.
struct LeaseLostStore(Arc<MemoryCheckpointer>);

#[async_trait]
impl Checkpointer for LeaseLostStore {
    async fn save(&self, _checkpoint: &Checkpoint) -> Result<String, GraphError> {
        Err(GraphError::CheckpointError(LEASE_LOST.to_owned()))
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
impl ParallelChildCheckpointerFactory for MemoryChildCheckpoints {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "test-execution".to_owned(),
            generation: 1,
        })
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Ok(format!(
            "test:{}:{}:{}:{}:{}:{}:{ordinal}:{input_digest:?}",
            activation.root_thread_id,
            activation.node_id,
            activation.step,
            origin.execution_id,
            origin.generation,
            branch.id(),
        ))
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        let thread_id = self.branch_thread_id(activation, branch, ordinal, input_digest, origin)?;
        let mut stores = self.stores.lock().await;
        let store = Arc::clone(
            stores
                .entry(thread_id.clone())
                .or_insert_with(|| Arc::new(MemoryCheckpointer::new())),
        );
        self.issued_threads.lock().await.push(thread_id.clone());
        let checkpointer: Arc<dyn Checkpointer> = if self.lease_lost_branch == Some(branch.id()) {
            Arc::new(LeaseLostStore(store))
        } else {
            store
        };
        Ok(ParallelChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer,
        })
    }
}

struct TestBranchGraphs {
    behaviors: HashMap<String, Behavior>,
    rejected_target: Option<String>,
}

#[async_trait]
impl ParallelBranchGraphFactory for TestBranchGraphs {
    fn owned_definition_digest(
        &self,
        branch: &ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError> {
        let mut result = [0; 32];
        result.copy_from_slice(
            ring::digest::digest(&ring::digest::SHA256, branch.node().as_bytes()).as_ref(),
        );
        Ok(result)
    }
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        if self.rejected_target.as_deref() == Some(branch.node()) {
            return Err(GraphError::InvalidGraph(
                "the branch definition is not an admitted Agent".to_owned(),
            ));
        }
        if !self.behaviors.contains_key(branch.node()) {
            return Err(GraphError::NodeNotFound(branch.node().to_owned()));
        }
        Ok(())
    }

    fn compile_branch(
        &self,
        branch: &ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let behavior = Arc::clone(
            self.behaviors
                .get(branch.node())
                .ok_or_else(|| GraphError::NodeNotFound(branch.node().to_owned()))?,
        );
        StateGraph::with_channels(&["branch_input", "branch_result"])
            .add_node(execution.wrap_node(FunctionNode::new("run", move |_| {
                let behavior = Arc::clone(&behavior);
                async move {
                    let value = behavior().await?;
                    Ok(NodeOutput::new().with_update("branch_result", value))
                }
            })))
            .add_edge(START, "run")
            .add_edge("run", adk_rust::graph::END)
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
            .ok_or_else(|| {
                GraphError::SerializationError("branch result projection is missing".to_owned())
            })
    }

    fn pause_cards(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        Err(GraphError::InvalidGraph(
            "this fixture does not produce pauses".to_owned(),
        ))
    }

    async fn resume_input(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
        _decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        Err(GraphError::InvalidGraph(
            "this fixture does not resume pauses".to_owned(),
        ))
    }
}

fn definition(branches: &[(&str, &str)], max_concurrency: usize) -> ParallelNodeDefinition {
    let branches = branches.iter().fold(String::new(), |mut yaml, (id, node)| {
        write!(yaml, "  - id: {id}\n    node: {node}\n").expect("write YAML fixture");
        yaml
    });
    ParallelNodeDefinition::from_yaml(&format!(
        "id: gather\ntype: parallel\nbranches:\n{branches}max_concurrency: {max_concurrency}\nwait: all\nerror_policy: fail_after_drain\noutput: [gathered]\ntransition: END\n"
    ))
    .expect("valid parallel definition")
}

fn runtime(
    checkpoints: Arc<MemoryChildCheckpoints>,
    behaviors: HashMap<String, Behavior>,
) -> Arc<AdkParallelBranchRuntime> {
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpoints.parent.clone();
    let checkpoint_factory: Arc<dyn ParallelChildCheckpointerFactory> = checkpoints;
    let graph_factory: Arc<dyn ParallelBranchGraphFactory> = Arc::new(TestBranchGraphs {
        behaviors,
        rejected_target: None,
    });
    Arc::new(AdkParallelBranchRuntime::new(
        checkpoint_factory,
        graph_factory,
        Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
    ))
}

fn parent_graph(
    definition: ParallelNodeDefinition,
    runtime: Arc<AdkParallelBranchRuntime>,
) -> CompiledGraph {
    let parent = runtime.parent_checkpointer();
    StateGraph::with_channels(&["input", "gathered", PARALLEL_RESUME_STATE_KEY])
        .add_node(DurableParallelNode::new(definition, runtime))
        .add_edge(START, "gather")
        .add_edge("gather", adk_rust::graph::END)
        .compile()
        .expect("compile parent graph")
        .with_checkpointer_arc(parent)
        .with_strict_channels()
}

fn constant(value: Value) -> Behavior {
    Arc::new(move || {
        let value = value.clone();
        Box::pin(async move { Ok(value) })
    })
}

#[test]
fn projected_input_digest_is_canonical_and_type_exact() {
    let mut nested_forward = serde_json::Map::new();
    nested_forward.insert("a".to_owned(), json!(1));
    nested_forward.insert("b".to_owned(), json!(2));
    let mut nested_reverse = serde_json::Map::new();
    nested_reverse.insert("b".to_owned(), json!(2));
    nested_reverse.insert("a".to_owned(), json!(1));

    let forward = HashMap::from([
        ("nested".to_owned(), Value::Object(nested_forward)),
        ("number".to_owned(), json!(1.0)),
    ]);
    let reverse = HashMap::from([
        ("number".to_owned(), json!(1.0)),
        ("nested".to_owned(), Value::Object(nested_reverse)),
    ]);
    let integer = HashMap::from([
        ("nested".to_owned(), json!({"a": 1, "b": 2})),
        ("number".to_owned(), json!(1)),
    ]);

    let digest = projected_input_digest(&forward).expect("digest canonical projected input");
    assert_eq!(
        digest,
        projected_input_digest(&reverse).expect("digest reordered projected input")
    );
    assert_ne!(
        digest,
        projected_input_digest(&integer).expect("digest integer projected input")
    );
    assert_eq!(
        digest,
        [
            56, 133, 28, 29, 234, 49, 17, 147, 54, 253, 80, 146, 101, 78, 38, 150, 197, 56, 159,
            224, 121, 75, 124, 245, 100, 59, 191, 160, 153, 46, 75, 143,
        ]
    );
}

#[test]
fn parallel_yaml_contract_is_strict_and_ui_shaped() {
    let definition = definition(&[("source_a", "fetch_a"), ("source_b", "fetch_b")], 2);

    assert_eq!(definition.id(), "gather");
    assert_eq!(definition.branches().len(), 2);
    assert_eq!(definition.max_concurrency(), 2);
    assert_eq!(definition.output_key(), "gathered");
    assert_eq!(definition.transition(), Some("END"));

    for unsupported in ["one", "many"] {
        let yaml = format!(
            "id: gather\ntype: parallel\nbranches:\n  - id: a\n    node: fetch_a\n  - id: b\n    node: fetch_b\nmax_concurrency: 2\nwait: {unsupported}\noutput: [gathered]\n"
        );
        assert!(ParallelNodeDefinition::from_yaml(&yaml).is_err());
    }
    for invalid in [
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: a, node: y}]\nmax_concurrency: 2\nwait: all\noutput: [gathered]\n",
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: b, node: y}]\nmax_concurrency: 3\nwait: all\noutput: [gathered]\n",
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: b, node: y}]\nmax_concurrency: 2\nwait: all\noutput: []\n",
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: b, node: y}]\nmax_concurrency: 2\nwait: all\noutput: [one, two]\n",
    ] {
        assert!(ParallelNodeDefinition::from_yaml(invalid).is_err());
    }

    let oversized_branches = (0..65).fold(String::new(), |mut yaml, index| {
        write!(yaml, "  - id: branch-{index}\n    node: node-{index}\n")
            .expect("write oversized YAML fixture");
        yaml
    });
    let oversized_yaml = format!(
        "id: gather\ntype: parallel\nbranches:\n{oversized_branches}max_concurrency: 2\nwait: all\noutput: [gathered]\n"
    );
    assert!(ParallelNodeDefinition::from_yaml(&oversized_yaml).is_err());
}

#[tokio::test]
async fn declared_order_is_stable_when_branches_finish_in_reverse_order() {
    let release_first = Arc::new(Notify::new());
    let first_release = Arc::clone(&release_first);
    let first: Behavior = Arc::new(move || {
        let first_release = Arc::clone(&first_release);
        Box::pin(async move {
            first_release.notified().await;
            Ok(json!({"value": "first"}))
        })
    });
    let second_release = Arc::clone(&release_first);
    let second: Behavior = Arc::new(move || {
        let second_release = Arc::clone(&second_release);
        Box::pin(async move {
            second_release.notify_one();
            Ok(json!({"value": "second"}))
        })
    });
    let graph = parent_graph(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([("first".to_owned(), first), ("second".to_owned(), second)]),
        ),
    );

    let result = graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("same"))]),
            ExecutionConfig::new("root-order"),
        )
        .await
        .expect("parallel graph result");

    assert_eq!(
        result["gathered"],
        json!([
            {"branch_id": "a", "node": "first", "outputs": {"value": "first"}},
            {"branch_id": "b", "node": "second", "outputs": {"value": "second"}},
        ])
    );
}

#[tokio::test]
async fn multiple_failures_drain_and_select_the_first_declared_branch() {
    let runs = Arc::new(AtomicUsize::new(0));
    let failing = |node: &'static str| {
        let runs = Arc::clone(&runs);
        Arc::new(move || {
            let runs = Arc::clone(&runs);
            Box::pin(async move {
                runs.fetch_add(1, Ordering::SeqCst);
                Err(GraphError::NodeExecutionFailed {
                    node: node.to_owned(),
                    message: "fixture failure".to_owned(),
                })
            }) as BehaviorFuture
        }) as Behavior
    };
    let graph = parent_graph(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([
                ("first".to_owned(), failing("first")),
                ("second".to_owned(), failing("second")),
            ]),
        ),
    );

    let error = graph
        .invoke(State::new(), ExecutionConfig::new("root-errors"))
        .await
        .expect_err("parallel graph must report a branch failure");

    assert_eq!(runs.load(Ordering::SeqCst), 2);
    match error {
        GraphError::NodeExecutionFailed { message, .. } => {
            assert!(message.contains("branch 'a'"));
            assert!(!message.contains("branch 'b'"));
        }
        other => panic!("unexpected parallel failure: {other}"),
    }
}

#[tokio::test]
async fn failure_stops_later_admission_at_concurrency_one() {
    let later_runs = Arc::new(AtomicUsize::new(0));
    let later_counter = Arc::clone(&later_runs);
    let later: Behavior = Arc::new(move || {
        let later_counter = Arc::clone(&later_counter);
        Box::pin(async move {
            later_counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"unexpected": true}))
        })
    });
    let first: Behavior = Arc::new(|| {
        Box::pin(async {
            Err(GraphError::NodeExecutionFailed {
                node: "first".to_owned(),
                message: "fixture failure".to_owned(),
            })
        })
    });
    let graph = parent_graph(
        definition(&[("first", "first"), ("later", "later")], 1),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([("first".to_owned(), first), ("later".to_owned(), later)]),
        ),
    );

    assert!(
        graph
            .invoke(State::new(), ExecutionConfig::new("root-stop-admission"))
            .await
            .is_err()
    );
    assert_eq!(later_runs.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn failure_drains_a_slow_inflight_sibling_without_admitting_pending_work() {
    let (entered_tx, mut entered_rx) = mpsc::channel(1);
    let release = Arc::new(Semaphore::new(0));
    let slow_release = Arc::clone(&release);
    let slow: Behavior = Arc::new(move || {
        let entered_tx = entered_tx.clone();
        let slow_release = Arc::clone(&slow_release);
        Box::pin(async move {
            entered_tx
                .send(())
                .await
                .map_err(|_| GraphError::Other("observer closed".to_owned()))?;
            slow_release
                .acquire()
                .await
                .map_err(|_| GraphError::Other("release closed".to_owned()))?
                .forget();
            Ok(json!({"drained": true}))
        })
    });
    let first: Behavior = Arc::new(|| {
        Box::pin(async {
            Err(GraphError::NodeExecutionFailed {
                node: "first".to_owned(),
                message: "fixture failure".to_owned(),
            })
        })
    });
    let pending_runs = Arc::new(AtomicUsize::new(0));
    let pending_counter = Arc::clone(&pending_runs);
    let pending: Behavior = Arc::new(move || {
        let pending_counter = Arc::clone(&pending_counter);
        Box::pin(async move {
            pending_counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"unexpected": true}))
        })
    });
    let graph = parent_graph(
        definition(
            &[("first", "first"), ("slow", "slow"), ("pending", "pending")],
            2,
        ),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([
                ("first".to_owned(), first),
                ("slow".to_owned(), slow),
                ("pending".to_owned(), pending),
            ]),
        ),
    );
    let run = tokio::spawn(async move {
        graph
            .invoke(State::new(), ExecutionConfig::new("root-inflight-drain"))
            .await
    });

    tokio::time::timeout(Duration::from_secs(1), entered_rx.recv())
        .await
        .expect("slow sibling entry wait")
        .expect("slow sibling entry");
    assert!(!run.is_finished());
    release.add_permits(1);
    assert!(run.await.expect("parallel graph task").is_err());
    assert_eq!(pending_runs.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn failed_branch_is_stable_and_changed_input_is_refused() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let short_runs = Arc::new(AtomicUsize::new(0));
    let short_counter = Arc::clone(&short_runs);
    let short: Behavior = Arc::new(move || {
        let short_counter = Arc::clone(&short_counter);
        Box::pin(async move {
            short_counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"value": "short"}))
        })
    });
    let long_runs = Arc::new(AtomicUsize::new(0));
    let long_counter = Arc::clone(&long_runs);
    let long: Behavior = Arc::new(move || {
        let long_counter = Arc::clone(&long_counter);
        Box::pin(async move {
            if long_counter.fetch_add(1, Ordering::SeqCst) == 0 {
                Err(GraphError::NodeExecutionFailed {
                    node: "long".to_owned(),
                    message: "fixture failure".to_owned(),
                })
            } else {
                Ok(json!({"value": "long"}))
            }
        })
    });
    let behaviors = HashMap::from([("short".to_owned(), short), ("long".to_owned(), long)]);
    let definition = definition(&[("short", "short"), ("long", "long")], 2);

    let first = parent_graph(
        definition.clone(),
        runtime(Arc::clone(&checkpoints), behaviors.clone()),
    );
    assert!(
        first
            .invoke(
                HashMap::from([("input".to_owned(), json!("original"))]),
                ExecutionConfig::new("root-restart"),
            )
            .await
            .is_err()
    );

    let recreated = parent_graph(
        definition.clone(),
        runtime(Arc::clone(&checkpoints), behaviors.clone()),
    );
    let same_input = recreated
        .invoke(
            HashMap::from([("input".to_owned(), json!("original"))]),
            ExecutionConfig::new("root-restart"),
        )
        .await;
    assert!(same_input.is_err());
    assert_eq!(short_runs.load(Ordering::SeqCst), 1);
    assert_eq!(long_runs.load(Ordering::SeqCst), 1);

    let changed = parent_graph(definition, runtime(checkpoints, behaviors))
        .invoke(
            HashMap::from([("input".to_owned(), json!("changed-after-restart"))]),
            ExecutionConfig::new("root-restart"),
        )
        .await;
    assert!(changed.is_err());
    assert_eq!(short_runs.load(Ordering::SeqCst), 1);
    assert_eq!(long_runs.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn admission_bound_is_structural_and_all_started_branches_drain() {
    const WIDTH: usize = 4;
    const LIMIT: usize = 2;
    let (entered_tx, mut entered_rx) = mpsc::channel(WIDTH);
    let release = Arc::new(Semaphore::new(0));
    let mut behaviors = HashMap::new();
    let mut branches = Vec::new();
    for index in 0..WIDTH {
        let entered = entered_tx.clone();
        let permits = Arc::clone(&release);
        let behavior: Behavior = Arc::new(move || {
            let entered = entered.clone();
            let permits = Arc::clone(&permits);
            Box::pin(async move {
                entered
                    .send(index)
                    .await
                    .map_err(|_| GraphError::Other("observer closed".to_owned()))?;
                permits
                    .acquire()
                    .await
                    .map_err(|_| GraphError::Other("release closed".to_owned()))?
                    .forget();
                Ok(json!({"index": index}))
            })
        });
        let name = format!("node-{index}");
        behaviors.insert(name.clone(), behavior);
        branches.push((format!("branch-{index}"), name));
    }
    drop(entered_tx);
    let branch_refs = branches
        .iter()
        .map(|(branch, node)| (branch.as_str(), node.as_str()))
        .collect::<Vec<_>>();
    let graph = parent_graph(
        definition(&branch_refs, LIMIT),
        runtime(Arc::new(MemoryChildCheckpoints::default()), behaviors),
    );
    let run = tokio::spawn(async move {
        graph
            .invoke(State::new(), ExecutionConfig::new("root-bounded"))
            .await
    });

    for _ in 0..LIMIT {
        tokio::time::timeout(Duration::from_secs(1), entered_rx.recv())
            .await
            .expect("bounded entry wait")
            .expect("entry observer");
    }
    assert!(entered_rx.try_recv().is_err());
    release.add_permits(LIMIT);
    for _ in LIMIT..WIDTH {
        tokio::time::timeout(Duration::from_secs(1), entered_rx.recv())
            .await
            .expect("next bounded entry wait")
            .expect("entry observer");
    }
    release.add_permits(WIDTH - LIMIT);

    let result = run
        .await
        .expect("parallel graph task")
        .expect("parallel result");
    assert_eq!(result["gathered"].as_array().map(Vec::len), Some(WIDTH));
}

#[tokio::test]
async fn loop_visits_get_distinct_child_checkpoint_threads() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runtime = runtime(
        Arc::clone(&checkpoints),
        HashMap::from([
            ("one".to_owned(), constant(json!({"value": 1}))),
            ("two".to_owned(), constant(json!({"value": 2}))),
        ]),
    );
    let node = DurableParallelNode::new(definition(&[("a", "one"), ("b", "two")], 2), runtime);

    node.execute(&NodeContext::new(
        State::new(),
        ExecutionConfig::new("root-loop"),
        3,
    ))
    .await
    .expect("first loop visit");
    // Simulate the owning parent's successful frontier advancement.
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpoints.parent.clone();
    parent
        .save(&adk_rust::graph::Checkpoint::new(
            "root-loop",
            State::new(),
            4,
            vec!["gather".to_owned()],
        ))
        .await
        .expect("parent advancement");
    node.execute(&NodeContext::new(
        State::new(),
        ExecutionConfig::new("root-loop"),
        7,
    ))
    .await
    .expect("second loop visit");

    let threads = checkpoints.issued_threads.lock().await;
    assert_eq!(threads.len(), 4);
    assert_ne!(threads[0], threads[2]);
    assert_ne!(threads[1], threads[3]);
}

struct PausingBranchGraphs {
    runs: Arc<HashMap<String, AtomicUsize>>,
    complete: BTreeSet<String>,
}

#[async_trait]
impl ParallelBranchGraphFactory for PausingBranchGraphs {
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        if self.runs.contains_key(branch.node()) {
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
        let runs = Arc::clone(&self.runs);
        let name = branch.node().to_owned();
        let node_name = name.clone();
        let complete = self.complete.contains(&name);
        let node = FunctionNode::new(&name, move |context| {
            let runs = Arc::clone(&runs);
            let name = node_name.clone();
            async move {
                runs[&name].fetch_add(1, Ordering::SeqCst);
                if !complete && !context.state.contains_key("__elitea_hitl_resume_v1") {
                    return Ok(NodeOutput::interrupt_with_data(
                        "Child paused.",
                        json!({"interrupt_id": format!("card-{name}"), "tool_call_id": format!("call-{name}")}),
                    ));
                }
                let blocked = context
                    .state
                    .get("__elitea_hitl_resume_v1")
                    .and_then(|value| value.get("action"))
                    .and_then(Value::as_str)
                    == Some("reject");
                Ok(NodeOutput::new()
                    .with_update("branch_result", json!({"output": name, "blocked": blocked})))
            }
        });
        StateGraph::with_channels(&["branch_input", "branch_result", "__elitea_hitl_resume_v1"])
            .add_node(execution.wrap_node(node))
            .add_edge(START, &name)
            .add_edge(&name, adk_rust::graph::END)
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
        let result = child
            .get("branch_result")
            .and_then(Value::as_object)
            .cloned()
            .ok_or_else(|| GraphError::Other("result missing".to_owned()))?;
        if result.get("blocked") == Some(&json!(true)) {
            Ok(ParallelBranchTerminal::Blocked)
        } else {
            Ok(ParallelBranchTerminal::Completed(result))
        }
    }

    fn pause_cards(
        &self,
        _branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        let adk_rust::graph::interrupt::Interrupt::Dynamic {
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
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        assert!(!pause.thread_id.is_empty());
        assert!(!pause.checkpoint_id.is_empty());
        assert_eq!(decisions.len(), 1);
        assert_eq!(decisions[0].interrupt_id, format!("card-{}", branch.node()));
        Ok(HashMap::from([(
            "__elitea_hitl_resume_v1".to_owned(),
            json!({"action": decisions[0].action}),
        )]))
    }
}

fn pausing_runtime(
    checkpoints: &Arc<MemoryChildCheckpoints>,
    runs: &Arc<HashMap<String, AtomicUsize>>,
    complete: &[&str],
) -> Arc<AdkParallelBranchRuntime> {
    let factory: Arc<dyn ParallelChildCheckpointerFactory> = checkpoints.clone();
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpoints.parent.clone();
    Arc::new(AdkParallelBranchRuntime::new(
        factory,
        Arc::new(PausingBranchGraphs {
            runs: Arc::clone(runs),
            complete: complete.iter().map(|name| (*name).to_owned()).collect(),
        }),
        Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
    ))
}

fn ready_branches(prepared: PreparedParallelActivation) -> Vec<PreparedParallelBranch> {
    match prepared {
        PreparedParallelActivation::Ready(branches) => branches,
        PreparedParallelActivation::Blocked(_) => panic!("unexpected blocked fixture"),
    }
}

fn stored_activation(checkpoint: &Checkpoint) -> ParallelActivation {
    serde_json::from_value(
        checkpoint.metadata["elitea.graph.parallel.occurrence.v2"]["activation"].clone(),
    )
    .unwrap()
}

fn pause_data(output: NodeOutput) -> Value {
    let Some(adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt
    else {
        panic!("expected aggregate pause");
    };
    assert_eq!(data["schema"], PARALLEL_INTERRUPT_SCHEMA);
    assert!(
        !serde_json::to_string(&data)
            .unwrap()
            .contains("checkpoint_id")
    );
    assert!(!serde_json::to_string(&data).unwrap().contains("thread_id"));
    data
}

fn resume_state(pause: &Value, input: &str) -> State {
    let decisions = pause["cards"].as_array().unwrap().iter().map(|card| json!({"interrupt_id": card["interrupt_id"], "tool_call_id": card["tool_call_id"], "action": "approve", "value": ""})).collect::<Vec<_>>();
    HashMap::from([
        ("input".to_owned(), json!(input)),
        (
            PARALLEL_RESUME_STATE_KEY.to_owned(),
            json!({"schema": PARALLEL_INTERRUPT_SCHEMA, "parallel_activation": pause["parallel_activation"], "decisions": decisions}),
        ),
    ])
}

#[tokio::test]
async fn child_pause_and_completion_survive_loss_before_parent_pause_save() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("done".to_owned(), AtomicUsize::new(0)),
        ("pause".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("done", "done"), ("pause", "pause")], 2);
    let runtime = pausing_runtime(&checkpoints, &runs, &["done"]);
    let context = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new("root-cutpoint"),
        4,
    );
    let mut activation = ParallelActivation {
        root_thread_id: "root-cutpoint".to_owned(),
        node_id: "gather".to_owned(),
        step: 4,
        config_digest: definition.config_digest(),
    };
    let prepared = ready_branches(
        runtime
            .prepare(&mut activation, &definition, &context)
            .await
            .unwrap(),
    );
    for branch in prepared {
        let _ = runtime.invoke(&activation, branch, &context).await;
    }
    // The parent has no published aggregate receipt at this cutpoint.
    let recreated = DurableParallelNode::new(
        definition.clone(),
        pausing_runtime(&checkpoints, &runs, &["done"]),
    );
    let pause = pause_data(recreated.execute(&context).await.unwrap());
    assert_eq!(pause["cards"].as_array().unwrap().len(), 1);
    assert_eq!(runs["done"].load(Ordering::SeqCst), 1);
    assert_eq!(runs["pause"].load(Ordering::SeqCst), 1);
    let resume = NodeContext::new(
        resume_state(&pause, "original"),
        ExecutionConfig::new("root-cutpoint"),
        4,
    );
    let output = recreated.execute(&resume).await.unwrap();
    assert_eq!(output.updates["gathered"][0]["outputs"]["output"], "done");
    assert_eq!(output.updates["gathered"][1]["outputs"]["output"], "pause");
    assert_eq!(runs["done"].load(Ordering::SeqCst), 1);
    assert_eq!(runs["pause"].load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn full_decision_redelivery_survives_one_resumed_child_completion() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let context = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new("root-resume-cutpoint"),
        2,
    );
    let pause = pause_data(
        DurableParallelNode::new(
            definition.clone(),
            pausing_runtime(&checkpoints, &runs, &[]),
        )
        .execute(&context)
        .await
        .unwrap(),
    );
    let resume = NodeContext::new(
        resume_state(&pause, "original"),
        ExecutionConfig::new("root-resume-cutpoint"),
        2,
    );
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let mut activation = ParallelActivation {
        root_thread_id: "root-resume-cutpoint".to_owned(),
        node_id: "gather".to_owned(),
        step: 2,
        config_digest: definition.config_digest(),
    };
    let mut prepared = ready_branches(
        runtime
            .prepare(&mut activation, &definition, &resume)
            .await
            .unwrap(),
    )
    .into_iter();
    let _ = runtime
        .invoke(&activation, prepared.next().unwrap(), &resume)
        .await;
    drop(prepared); // Process loss before the second admitted child runs.
    let output = DurableParallelNode::new(definition, pausing_runtime(&checkpoints, &runs, &[]))
        .execute(&resume)
        .await
        .unwrap();
    assert!(output.interrupt.is_none());
    assert_eq!(output.updates["gathered"].as_array().unwrap().len(), 2);
    assert_eq!(runs["a"].load(Ordering::SeqCst), 2);
    assert_eq!(runs["b"].load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn partial_duplicate_stale_foreign_and_changed_input_decisions_run_no_child() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let node = DurableParallelNode::new(definition, pausing_runtime(&checkpoints, &runs, &[]));
    let context = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new("root-invalid-decisions"),
        0,
    );
    let pause = pause_data(node.execute(&context).await.unwrap());
    for variation in 0..5 {
        let mut state = resume_state(&pause, "original");
        let decisions = state[PARALLEL_RESUME_STATE_KEY]["decisions"]
            .as_array()
            .unwrap()
            .clone();
        match variation {
            0 => {
                state.get_mut(PARALLEL_RESUME_STATE_KEY).unwrap()["decisions"] =
                    json!([decisions[0]]);
            }
            1 => {
                state.get_mut(PARALLEL_RESUME_STATE_KEY).unwrap()["decisions"] =
                    json!([decisions[0], decisions[0]]);
            }
            2 => {
                state.get_mut(PARALLEL_RESUME_STATE_KEY).unwrap()["parallel_activation"] =
                    json!("p1:stale");
            }
            3 => {
                state.get_mut(PARALLEL_RESUME_STATE_KEY).unwrap()["decisions"][0]["interrupt_id"] =
                    json!("foreign-card");
            }
            4 => {
                state.insert("input".to_owned(), json!("changed"));
            }
            _ => unreachable!(),
        }
        assert!(
            node.execute(&NodeContext::new(
                state,
                ExecutionConfig::new("root-invalid-decisions"),
                0
            ))
            .await
            .is_err()
        );
        assert_eq!(runs["a"].load(Ordering::SeqCst), 1);
        assert_eq!(runs["b"].load(Ordering::SeqCst), 1);
    }
}

#[tokio::test]
async fn a_blocked_branch_is_typed_and_the_adk_parent_cannot_report_success() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let parent = runtime.parent_checkpointer();
    let successor_runs = Arc::new(AtomicUsize::new(0));
    let successor_counter = Arc::clone(&successor_runs);
    let graph = StateGraph::with_channels(&["input", "gathered", PARALLEL_RESUME_STATE_KEY])
        .add_node(DurableParallelNode::new(definition.clone(), runtime))
        .add_node(FunctionNode::new("after", move |_| {
            let counter = Arc::clone(&successor_counter);
            async move {
                counter.fetch_add(1, Ordering::SeqCst);
                Ok(NodeOutput::new())
            }
        }))
        .add_edge(START, "gather")
        .add_edge("gather", "after")
        .add_edge("after", adk_rust::graph::END)
        .compile()
        .unwrap()
        .with_checkpointer_arc(parent);
    let thread = "root-blocked";
    let interrupted = graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("original"))]),
            ExecutionConfig::new(thread),
        )
        .await
        .unwrap_err();
    let GraphError::Interrupted(interrupted) = interrupted else {
        panic!("expected parent pause")
    };
    let adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(pause), ..
    } = interrupted.interrupt
    else {
        panic!("expected aggregate pause")
    };
    let mut state = resume_state(&pause, "original");
    state.get_mut(PARALLEL_RESUME_STATE_KEY).unwrap()["decisions"][0]["action"] = json!("reject");
    let error = graph
        .invoke(
            state.clone(),
            ExecutionConfig::new(thread).with_resume_from(&interrupted.checkpoint_id),
        )
        .await
        .unwrap_err();
    assert!(matches!(error, GraphError::NodeExecutionFailed { .. }));
    assert!(error.to_string().contains("graph.parallel.blocked"));
    assert_eq!(successor_runs.load(Ordering::SeqCst), 0);
    let saved = checkpoints.parent.load(thread).await.unwrap().unwrap();
    assert!(!saved.state.contains_key("gathered"));
    assert_eq!(saved.pending_nodes, ["gather"]);
    let activation = stored_activation(&saved);
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let expected = ParallelBlocked {
        branch_id: "a".to_owned(),
        node: "a".to_owned(),
        ordinal: 0,
    };
    assert_eq!(
        runtime
            .occurrence_checkpointer()
            .blocked_at(&saved.checkpoint_id, &activation)
            .await
            .unwrap(),
        Some(expected.clone())
    );
    let node = DurableParallelNode::new(definition, runtime);
    let outcome = node
        .execute_outcome(&NodeContext::new(
            state,
            ExecutionConfig::new(thread),
            saved.step,
        ))
        .await
        .unwrap();
    assert!(matches!(outcome, ParallelNodeOutcome::Blocked(blocked) if blocked == expected));
    assert_eq!(runs["a"].load(Ordering::SeqCst), 2);
    assert_eq!(runs["b"].load(Ordering::SeqCst), 2);
}

#[tokio::test]
async fn freeze_rejects_a_competing_parent_before_any_child_is_admitted() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let thread = "root-freeze-race";
    let original = Checkpoint::new(
        thread,
        HashMap::from([("input".to_owned(), json!("original"))]),
        0,
        vec!["gather".to_owned()],
    );
    checkpoints.parent.save(&original).await.unwrap();
    let competing = Checkpoint::new(
        thread,
        HashMap::from([("input".to_owned(), json!("newer"))]),
        1,
        vec!["after".to_owned()],
    );
    checkpoints.parent.rows.lock().await.before_append = Some(competing.clone());
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let node = DurableParallelNode::new(
        definition,
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("a".to_owned(), constant(json!({"output": "a"}))),
                ("b".to_owned(), constant(json!({"output": "b"}))),
            ]),
        ),
    );
    assert!(
        node.execute(&NodeContext::new(
            original.state,
            ExecutionConfig::new(thread),
            0
        ))
        .await
        .is_err()
    );
    assert!(checkpoints.issued_threads.lock().await.is_empty());
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &competing
        )
        .unwrap()
    );
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 2);
}

#[tokio::test]
async fn a_pause_receipt_cannot_attach_to_an_advanced_parent_frontier() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let context = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new("root-pause-race"),
        0,
    );
    let mut activation = ParallelActivation {
        root_thread_id: context.config.thread_id.clone(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    for branch in ready_branches(
        runtime
            .prepare(&mut activation, &definition, &context)
            .await
            .unwrap(),
    ) {
        let _ = runtime.invoke(&activation, branch, &context).await;
    }
    let competing = Checkpoint::new(
        &context.config.thread_id,
        HashMap::from([("input".to_owned(), json!("newer"))]),
        1,
        vec!["after".to_owned()],
    );
    checkpoints.parent.rows.lock().await.before_append = Some(competing.clone());
    assert!(
        DurableParallelNode::new(definition, runtime)
            .execute(&context)
            .await
            .is_err()
    );
    let latest = checkpoints
        .parent
        .load(&context.config.thread_id)
        .await
        .unwrap()
        .unwrap();
    assert!(checkpoint_equal(&latest, &competing).unwrap());
    assert!(
        !latest
            .metadata
            .contains_key("elitea.graph.parallel.occurrence.v2")
    );
    assert_eq!(runs["a"].load(Ordering::SeqCst), 1);
    assert_eq!(runs["b"].load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn decisions_cannot_overwrite_competing_business_state_or_receipts() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let node = DurableParallelNode::new(definition, pausing_runtime(&checkpoints, &runs, &[]));
    let thread = "root-decision-race";
    let context = NodeContext::new(
        HashMap::from([
            ("input".to_owned(), json!("original")),
            ("unmapped".to_owned(), json!("preserved")),
        ]),
        ExecutionConfig::new(thread),
        0,
    );
    let pause = pause_data(node.execute(&context).await.unwrap());
    let original = checkpoints.parent.load(thread).await.unwrap().unwrap();
    let mut missing_business = resume_state(&pause, "original");
    assert!(
        node.execute(&NodeContext::new(
            missing_business.clone(),
            ExecutionConfig::new(thread),
            0
        ))
        .await
        .is_err()
    );
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &original
        )
        .unwrap()
    );
    missing_business.insert("unmapped".to_owned(), json!("preserved"));
    let mut competing = original.clone();
    let fresh = Checkpoint::new(
        thread,
        original.state.clone(),
        original.step,
        original.pending_nodes.clone(),
    );
    competing.checkpoint_id = fresh.checkpoint_id;
    competing.created_at = fresh.created_at;
    competing
        .state
        .insert("unmapped".to_owned(), json!("newer"));
    competing
        .metadata
        .insert("unrelated_receipt".to_owned(), json!("newer"));
    checkpoints.parent.rows.lock().await.before_append = Some(competing.clone());
    assert!(
        node.execute(&NodeContext::new(
            missing_business,
            ExecutionConfig::new(thread),
            0
        ))
        .await
        .is_err()
    );
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &competing
        )
        .unwrap()
    );
    assert!(competing.metadata["elitea.graph.parallel.occurrence.v2"]["decisions"].is_null());
    assert_eq!(runs["a"].load(Ordering::SeqCst), 1);
    assert_eq!(runs["b"].load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn immutable_pre_freeze_replay_stays_unchanged_and_does_not_replace_latest() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let thread = "root-immutable-replay";
    let original = Checkpoint::new(
        thread,
        HashMap::from([("input".to_owned(), json!("original"))]),
        0,
        vec!["gather".to_owned()],
    );
    checkpoints.parent.save(&original).await.unwrap();
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runtime = runtime(
        Arc::clone(&checkpoints),
        HashMap::from([
            ("a".to_owned(), constant(json!({"output": "a"}))),
            ("b".to_owned(), constant(json!({"output": "b"}))),
        ]),
    );
    let context = NodeContext::new(original.state.clone(), ExecutionConfig::new(thread), 0);
    let mut activation = ParallelActivation {
        root_thread_id: thread.to_owned(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    let _ = runtime
        .prepare(&mut activation, &definition, &context)
        .await
        .unwrap();
    let frozen = checkpoints.parent.load(thread).await.unwrap().unwrap();
    assert_ne!(frozen.checkpoint_id, original.checkpoint_id);
    assert_eq!(
        runtime.parent_checkpointer().save(&original).await.unwrap(),
        original.checkpoint_id
    );
    assert!(
        checkpoint_equal(
            &checkpoints
                .parent
                .load_by_id(&original.checkpoint_id)
                .await
                .unwrap()
                .unwrap(),
            &original
        )
        .unwrap()
    );
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &frozen
        )
        .unwrap()
    );
    let mut conflicting = original;
    conflicting
        .state
        .insert("input".to_owned(), json!("changed"));
    assert!(
        runtime
            .parent_checkpointer()
            .save(&conflicting)
            .await
            .is_err()
    );
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 2);
}

struct DropObserver(Arc<AtomicUsize>);

impl Drop for DropObserver {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

#[tokio::test(start_paused = true)]
async fn deadline_stops_admission_and_owned_future_cleanup_is_bounded() {
    let entered = Arc::new(AtomicUsize::new(0));
    let dropped = Arc::new(AtomicUsize::new(0));
    let behavior: Behavior = {
        let entered = Arc::clone(&entered);
        let dropped = Arc::clone(&dropped);
        Arc::new(move || {
            let entered = Arc::clone(&entered);
            let dropped = Arc::clone(&dropped);
            Box::pin(async move {
                entered.fetch_add(1, Ordering::SeqCst);
                let _observer = DropObserver(dropped);
                std::future::pending::<Result<Value, GraphError>>().await
            })
        })
    };
    let runtime = runtime(
        Arc::new(MemoryChildCheckpoints::default()),
        HashMap::from([
            ("a".to_owned(), Arc::clone(&behavior)),
            ("b".to_owned(), Arc::clone(&behavior)),
            ("later".to_owned(), behavior),
        ]),
    );
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b"), ("later", "later")], 2),
        runtime,
    )
    .with_deadline(tokio::time::Instant::now() + Duration::from_secs(1));
    let error = node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("root-deadline"),
            0,
        ))
        .await
        .err()
        .expect("parallel cancellation error");
    assert!(
        error
            .to_string()
            .contains("graph.parallel.cancellation_cleanup_failed")
    );
    assert_eq!(entered.load(Ordering::SeqCst), 2);
    assert_eq!(dropped.load(Ordering::SeqCst), 2);
}

#[test]
fn unadmitted_branch_is_rejected_before_the_parent_graph_runs() {
    let definition = definition(&[("a", "safe"), ("b", "pause")], 2);
    let checkpoint_factory: Arc<dyn ParallelChildCheckpointerFactory> =
        Arc::new(MemoryChildCheckpoints::default());
    let graph_factory: Arc<dyn ParallelBranchGraphFactory> = Arc::new(TestBranchGraphs {
        behaviors: HashMap::from([
            ("safe".to_owned(), constant(json!({"ok": true}))),
            ("pause".to_owned(), constant(json!({"unused": true}))),
        ]),
        rejected_target: Some("pause".to_owned()),
    });
    let runtime = Arc::new(AdkParallelBranchRuntime::new(
        checkpoint_factory,
        graph_factory,
        Arc::new(ParallelOccurrenceCheckpointer::new(Arc::new(
            AtomicParentCheckpoints::default(),
        ))),
    ));

    assert!(
        StateGraph::with_channels(&["gathered"])
            .add_node(DurableParallelNode::new(definition, runtime))
            .add_edge(START, "gather")
            .add_edge("gather", adk_rust::graph::END)
            .compile()
            .is_err()
    );
}

#[tokio::test]
async fn new_parallel_revision_consumes_previous_graph_call_marker_and_retains_receipt_bytes() {
    use super::parallel::ParallelBranchRuntime;
    use crate::agents::pipeline::scope_receipts::{
        GRAPH_CALL_RECEIPTS_METADATA_KEY, GRAPH_CALL_REVISION_METADATA_KEY,
    };
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let state = State::from([("input".to_owned(), json!("original"))]);
    let mut previous = adk_rust::graph::Checkpoint::new(
        "receipt-marker",
        state.clone(),
        0,
        vec!["gather".to_owned()],
    );
    let original = json!({"retained": "opaque exact original bytes"});
    previous.metadata.insert(
        GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(),
        original.clone(),
    );
    previous.metadata.insert(
        GRAPH_CALL_REVISION_METADATA_KEY.to_owned(),
        json!({"parent": "old-frontier"}),
    );
    checkpoints.parent.save(&previous).await.unwrap();
    let mut activation = ParallelActivation {
        root_thread_id: "receipt-marker".to_owned(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    runtime
        .prepare(
            &mut activation,
            &definition,
            &NodeContext::new(state, ExecutionConfig::new("receipt-marker"), 0),
        )
        .await
        .unwrap();
    let latest = checkpoints
        .parent
        .load("receipt-marker")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(latest.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY], original);
    assert!(
        !latest
            .metadata
            .contains_key(GRAPH_CALL_REVISION_METADATA_KEY)
    );
    assert_ne!(latest.checkpoint_id, previous.checkpoint_id);
    assert_eq!(
        checkpoints
            .parent
            .load_by_id(&previous.checkpoint_id)
            .await
            .unwrap()
            .unwrap()
            .metadata,
        previous.metadata
    );
}

#[test]
fn structural_input_bounds_precede_recursive_hash() {
    let mut deep = Value::Null;
    for _ in 0..256 {
        deep = Value::Array(vec![deep]);
    }
    assert!(projected_input_digest(&State::from([("input".into(), deep)])).is_err());
}

#[tokio::test]
async fn structural_parent_rejection_precedes_checkpoint_mint_and_child_effects() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(AtomicUsize::new(0));
    let make_behavior = || {
        let runs = Arc::clone(&runs);
        Arc::new(move || {
            let runs = Arc::clone(&runs);
            Box::pin(async move {
                runs.fetch_add(1, Ordering::SeqCst);
                Ok(json!({"value": "should not run"}))
            }) as BehaviorFuture
        }) as Behavior
    };
    let node = DurableParallelNode::new(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("first".into(), make_behavior()),
                ("second".into(), make_behavior()),
            ]),
        ),
    );
    let mut deep = Value::Null;
    for _ in 0..256 {
        deep = Value::Array(vec![deep]);
    }
    let context = NodeContext::new(
        State::from([("input".into(), deep)]),
        ExecutionConfig::new("deep-parent"),
        0,
    );
    assert!(node.execute_outcome(&context).await.is_err());
    assert_eq!(runs.load(Ordering::SeqCst), 0);
    assert!(checkpoints.issued_threads.lock().await.is_empty());
    assert!(
        checkpoints
            .parent
            .list("deep-parent")
            .await
            .unwrap()
            .is_empty()
    );
}

#[tokio::test]
async fn a_legacy_occurrence_without_frozen_child_identity_is_refused_by_type() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let thread = "root-legacy";
    let state = HashMap::from([("input".to_owned(), json!("original"))]);
    let mut legacy = Checkpoint::new(thread, state.clone(), 3, vec!["gather".to_owned()]);
    legacy.metadata.insert(
        "elitea.graph.parallel.occurrence.v1".to_owned(),
        json!({"activation": {}, "branches": []}),
    );
    checkpoints.parent.save(&legacy).await.unwrap();
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b")], 2),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("a".to_owned(), constant(json!({"output": "a"}))),
                ("b".to_owned(), constant(json!({"output": "b"}))),
            ]),
        ),
    );
    let error = node
        .execute(&NodeContext::new(state, ExecutionConfig::new(thread), 3))
        .await
        .err()
        .expect("a legacy occurrence is refused");
    assert!(
        error
            .to_string()
            .contains("graph.parallel.unsupported_occurrence")
    );
    assert!(checkpoints.issued_threads.lock().await.is_empty());
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 1);
}

fn pending_behavior(
    entered: &Arc<AtomicUsize>,
    dropped: &Arc<AtomicUsize>,
    announce: Option<mpsc::UnboundedSender<()>>,
) -> Behavior {
    let entered = Arc::clone(entered);
    let dropped = Arc::clone(dropped);
    Arc::new(move || {
        let entered = Arc::clone(&entered);
        let dropped = Arc::clone(&dropped);
        let announce = announce.clone();
        Box::pin(async move {
            entered.fetch_add(1, Ordering::SeqCst);
            let _observer = DropObserver(dropped);
            if let Some(announce) = announce {
                let _ = announce.send(());
            }
            std::future::pending::<Result<Value, GraphError>>().await
        })
    })
}

fn pending_runtime(behavior: &Behavior, names: &[&str]) -> Arc<AdkParallelBranchRuntime> {
    runtime(
        Arc::new(MemoryChildCheckpoints::default()),
        names
            .iter()
            .map(|name| ((*name).to_owned(), Arc::clone(behavior)))
            .collect(),
    )
}

fn root_context(thread: &str) -> NodeContext {
    NodeContext::new(State::new(), ExecutionConfig::new(thread), 0)
}

async fn failed_receipt_count(checkpoints: &MemoryChildCheckpoints) -> usize {
    let stores = checkpoints.stores.lock().await;
    let mut failed = 0;
    for (thread, store) in stores.iter() {
        for saved in store.list(thread).await.expect("list child checkpoints") {
            let metadata = serde_json::to_string(&saved.metadata).expect("encode metadata");
            if metadata.contains("\"status\":\"failed\"") {
                failed += 1;
            }
        }
    }
    failed
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn latch_cancel_stops_inflight_branches_within_the_cleanup_bound_and_admits_nothing_new() {
    let entered = Arc::new(AtomicUsize::new(0));
    let dropped = Arc::new(AtomicUsize::new(0));
    let (announce, mut entries) = mpsc::unbounded_channel();
    let behavior = pending_behavior(&entered, &dropped, Some(announce));
    let latch = Arc::new(FanoutCancellation::new());
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b"), ("later", "later")], 2),
        pending_runtime(&behavior, &["a", "b", "later"]),
    )
    .with_cancellation(Arc::clone(&latch))
    .with_cleanup_timeout(Duration::from_millis(20));
    let run = tokio::spawn(async move { node.execute(&root_context("root-latch")).await });
    entries.recv().await.expect("first branch entered");
    entries.recv().await.expect("second branch entered");

    let fired = std::time::Instant::now();
    latch.cancel();
    let error = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("the latch must stop the node")
        .expect("node task")
        .err()
        .expect("parallel cancellation error");
    let elapsed = fired.elapsed();
    eprintln!("latch_cancel_latency: {elapsed:?}");

    assert!(error.to_string().contains("graph.parallel.cancel"));
    assert!(elapsed <= Duration::from_millis(100), "{elapsed:?}");
    assert_eq!(dropped.load(Ordering::SeqCst), 2);
    assert_eq!(
        entered.load(Ordering::SeqCst),
        2,
        "a branch was admitted after cancel"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn deadline_stops_non_cooperative_branches_at_the_deadline_plus_cleanup() {
    let entered = Arc::new(AtomicUsize::new(0));
    let dropped = Arc::new(AtomicUsize::new(0));
    let behavior = pending_behavior(&entered, &dropped, None);
    let started = std::time::Instant::now();
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b"), ("later", "later")], 2),
        pending_runtime(&behavior, &["a", "b", "later"]),
    )
    .with_deadline(tokio::time::Instant::now() + Duration::from_millis(50))
    .with_cleanup_timeout(Duration::from_millis(20));
    let result = tokio::time::timeout(
        Duration::from_secs(5),
        node.execute(&root_context("root-deadline-live")),
    )
    .await
    .expect("the deadline must stop the node");
    let elapsed = started.elapsed();
    eprintln!("deadline_honored: {elapsed:?}");

    assert!(result.is_err());
    assert!(elapsed >= Duration::from_millis(50), "{elapsed:?}");
    assert!(elapsed <= Duration::from_millis(170), "{elapsed:?}");
    assert_eq!(entered.load(Ordering::SeqCst), 2);
    assert_eq!(dropped.load(Ordering::SeqCst), 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_lease_lost_branch_is_a_control_stop_not_a_recorded_failure() {
    let checkpoints = Arc::new(MemoryChildCheckpoints {
        lease_lost_branch: Some("a"),
        ..MemoryChildCheckpoints::default()
    });
    let second_runs = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&second_runs);
    let second: Behavior = Arc::new(move || {
        let counter = Arc::clone(&counter);
        Box::pin(async move {
            counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"value": "second"}))
        })
    });
    let node = DurableParallelNode::new(
        definition(&[("a", "first"), ("b", "second")], 1),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("first".to_owned(), constant(json!({"value": "first"}))),
                ("second".to_owned(), second),
            ]),
        ),
    );
    let error = node
        .execute(&root_context("root-lease"))
        .await
        .err()
        .expect("lease loss must stop the node");

    assert!(
        matches!(&error, GraphError::CheckpointError(message) if message == LEASE_LOST),
        "{error}"
    );
    assert!(!error.to_string().contains("branch_failed"));
    assert_eq!(
        second_runs.load(Ordering::SeqCst),
        0,
        "admitted after lease loss"
    );
    assert_eq!(failed_receipt_count(&checkpoints).await, 0);
    let parent = checkpoints
        .parent
        .load("root-lease")
        .await
        .expect("load parent")
        .expect("frozen occurrence");
    let occurrence = &parent.metadata["elitea.graph.parallel.occurrence.v2"];
    assert!(occurrence["blocked"].is_null());
    assert_eq!(occurrence["cards"], json!([]));
}

struct CancellableInvocation {
    cancelled: Arc<std::sync::atomic::AtomicBool>,
    content: adk_rust::Content,
    config: adk_rust::RunConfig,
}

#[async_trait]
#[allow(clippy::unnecessary_literal_bound)] // Fixed by the foreign trait signature.
impl adk_rust::ReadonlyContext for CancellableInvocation {
    fn invocation_id(&self) -> &str {
        "inv"
    }
    fn agent_name(&self) -> &str {
        "agent"
    }
    fn user_id(&self) -> &str {
        "user"
    }
    fn app_name(&self) -> &str {
        "app"
    }
    fn session_id(&self) -> &str {
        "session"
    }
    fn branch(&self) -> &str {
        ""
    }
    fn user_content(&self) -> &adk_rust::Content {
        &self.content
    }
}

#[async_trait]
impl adk_rust::CallbackContext for CancellableInvocation {
    fn artifacts(&self) -> Option<Arc<dyn adk_rust::Artifacts>> {
        None
    }
}

struct NoSession;

#[allow(clippy::unnecessary_literal_bound)] // Fixed by the foreign trait signature.
impl adk_rust::Session for NoSession {
    fn id(&self) -> &str {
        "session"
    }
    fn app_name(&self) -> &str {
        "app"
    }
    fn user_id(&self) -> &str {
        "user"
    }
    fn state(&self) -> &dyn adk_rust::State {
        &NoState
    }
    fn conversation_history(&self) -> Vec<adk_rust::Content> {
        Vec::new()
    }
}

struct NoState;

impl adk_rust::State for NoState {
    fn get(&self, _key: &str) -> Option<Value> {
        None
    }
    fn set(&mut self, _key: String, _value: Value) {}
    fn all(&self) -> HashMap<String, Value> {
        HashMap::new()
    }
}

#[async_trait]
impl adk_rust::InvocationContext for CancellableInvocation {
    fn agent(&self) -> Arc<dyn adk_rust::Agent> {
        unreachable!("the parallel runner never reads the parent agent")
    }
    fn memory(&self) -> Option<Arc<dyn adk_rust::Memory>> {
        None
    }
    fn session(&self) -> &dyn adk_rust::Session {
        &NoSession
    }
    fn run_config(&self) -> &adk_rust::RunConfig {
        &self.config
    }
    fn end_invocation(&self) {}
    fn ended(&self) -> bool {
        false
    }
    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::SeqCst)
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_branch_failing_after_cancellation_writes_no_failed_receipt() {
    let cancelled = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let flag = Arc::clone(&cancelled);
    let failing: Behavior = Arc::new(move || {
        let flag = Arc::clone(&flag);
        Box::pin(async move {
            flag.store(true, Ordering::SeqCst);
            Err(GraphError::NodeExecutionFailed {
                node: "run".to_owned(),
                message: "interrupted by cancellation".to_owned(),
            })
        })
    });
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b")], 1),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("a".to_owned(), failing),
                ("b".to_owned(), constant(json!({"value": "b"}))),
            ]),
        ),
    );
    let parent: Arc<dyn adk_rust::InvocationContext> = Arc::new(CancellableInvocation {
        cancelled,
        content: adk_rust::Content::new("user"),
        config: adk_rust::RunConfig::default(),
    });
    let config = ExecutionConfig::new("root-cancelled-failure").with_parent_context(parent);
    let error = node
        .execute(&NodeContext::new(State::new(), config, 0))
        .await
        .err()
        .expect("cancelled parallel node");

    assert!(
        error.to_string().contains("graph.parallel.cancelled"),
        "{error}"
    );
    assert_eq!(failed_receipt_count(&checkpoints).await, 0);
}
